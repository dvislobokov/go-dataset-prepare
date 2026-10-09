package semantic

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"

	"goflc/internal/flc"
)

// Run is the semantic pass over a finished syntax run directory: it writes semantic.jsonl and rewrites samples.jsonl
// with semantic_status/semantic_reason mirrored from the editor_snapshot record. One Loader per repository run, so
// std/module imports are type-checked once (declarations only). Packages are analyzed by `workers` goroutines;
// records are written in sorted package order (deterministic, and flushed per package so an interrupted run can be
// salvaged).
func Run(dir string, cfg *flc.Config, repo *flc.RepoInfo, workers int) (map[string]int, error) {
	samples, gz, err := readSamples(dir)
	if err != nil {
		return nil, err
	}
	if workers < 1 {
		workers = 1
	}
	// Short-lived, allocation-heavy pass: trade memory for less GC (bounded by GOMEMLIMIT when the orchestrator sets it).
	defer debug.SetGCPercent(debug.SetGCPercent(300))
	counters := map[string]int{}
	l := NewLoader(repo.Root, repo.ModulePath, cfg.Discovery.BuildContext.GOOS, cfg.Discovery.BuildContext.GOARCH,
		cfg.Discovery.BuildContext.Tags, cfg.Semantic.UseVendor)
	lim := limits{maxScope: cfg.Semantic.MaxScopeSymbols, maxMembers: cfg.Semantic.MaxMembers, maxTypes: 6, maxTypeMembers: 10}
	out, err := flc.NewJSONLWriter(filepath.Join(dir, "semantic.jsonl"), gz)
	if err != nil {
		return nil, err
	}
	byPkg := map[string][]int{}
	for i, s := range samples {
		byPkg[s.Project] = append(byPkg[s.Project], i)
	}
	pkgs := make([]string, 0, len(byPkg))
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	// Work items are chunks of a package's samples; all chunks of a package share one pkgCache (original check,
	// stripped ASTs) built once under its lock. Items are written in (package, chunk) order: deterministic.
	const chunk = 48
	type item struct {
		cache    *pkgCache
		relDir   string
		idxs     []int
		recs     []*Record
		ridx     []int
		counters map[string]int
		done     chan struct{}
	}
	var items []*item
	for _, p := range pkgs {
		pc := &pkgCache{}
		all := byPkg[p]
		for i := 0; i < len(all); i += chunk {
			j := min(i+chunk, len(all))
			items = append(items, &item{cache: pc, relDir: p, idxs: all[i:j], counters: map[string]int{}, done: make(chan struct{})})
		}
	}
	jobs := make(chan *item)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				it.recs, it.ridx = analyzeSamples(l, cfg, repo, samples, it.cache, it.relDir, it.idxs, lim, it.counters)
				close(it.done)
			}
		}()
	}
	go func() {
		for _, it := range items {
			jobs <- it
		}
		close(jobs)
	}()
	status := make([]*Record, len(samples))
	var werr error
	for _, r := range items {
		<-r.done
		for k, v := range r.counters {
			counters[k] += v
		}
		for j, rec := range r.recs {
			i := r.ridx[j]
			if werr == nil {
				werr = out.Write(rec)
			}
			if rec.VisibilityPolicy == "editor_snapshot" || (status[i] == nil && len(cfg.Semantic.Policies) == 1) {
				status[i] = rec
			}
		}
		r.recs = nil
		if werr == nil {
			werr = out.Flush()
		}
	}
	wg.Wait()
	if werr != nil {
		return nil, werr
	}
	if err := out.Close(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	for k, v := range l.Stats {
		counters["semantic.loader."+k] = v
	}
	counters["semantic.loader.unresolved_import_paths"] = len(l.Unresolved)
	l.mu.Unlock()
	// rewrite samples with mirrored status (required mode drops samples without a resolved/partial record)
	w, err := flc.NewJSONLWriter(filepath.Join(dir, "samples.jsonl.sem-tmp"), false)
	if err != nil {
		return nil, err
	}
	for i, s := range samples {
		if r := status[i]; r != nil {
			s.SemanticStatus, s.SemanticReason = r.Status, r.Reason
		} else {
			s.SemanticStatus, s.SemanticReason = "not_attempted", strPtr("not_in_subset")
		}
		if cfg.Semantic.Mode == "required" && s.SemanticStatus != "resolved" && s.SemanticStatus != "partially_resolved" {
			counters["semantic.required_dropped"]++
			continue
		}
		if err := w.Write(s); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	tmp := filepath.Join(dir, "samples.jsonl.sem-tmp")
	if gz {
		b, err := os.ReadFile(tmp)
		if err != nil {
			return nil, err
		}
		gw, err := flc.NewJSONLWriter(filepath.Join(dir, "samples.jsonl"), true)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		sc.Buffer(make([]byte, 1<<20), 1<<28)
		for sc.Scan() {
			gw.Write(json.RawMessage(append([]byte{}, sc.Bytes()...)))
		}
		gw.Close()
		return counters, os.Remove(tmp)
	}
	return counters, os.Rename(tmp, filepath.Join(dir, "samples.jsonl"))
}

// pkgCache holds per-package state shared by the chunks of one package directory.
type pkgCache struct {
	mu       sync.Mutex
	listed   bool
	pf       *PkgFiles
	parsed   map[string]map[string]*ast.File // variant -> abs path -> body-stripped AST (snapshot engine)
	origPkgs map[string]*origPkg             // variant -> cached original package (original_scope engine)
	origAST  map[string]*ast.File            // original file ASTs for decl_splice
	srcs     map[string][]byte
}

func (pc *pkgCache) init(l *Loader, absDir string) {
	if !pc.listed {
		pc.listed = true
		pc.pf, _ = l.ListDir(absDir)
		pc.parsed, pc.origPkgs = map[string]map[string]*ast.File{}, map[string]*origPkg{}
		pc.origAST, pc.srcs = map[string]*ast.File{}, map[string][]byte{}
	}
}

// analyzeSamples computes the records of a chunk of one package's samples (deterministic order).
func analyzeSamples(l *Loader, cfg *flc.Config, repo *flc.RepoInfo, samples []*flc.Sample, pc *pkgCache, relDir string,
	idxs []int, lim limits, counters map[string]int) ([]*Record, []int) {
	absDir := filepath.Join(repo.Root, filepath.FromSlash(relDir))
	pc.mu.Lock()
	pc.init(l, absDir)
	pf := pc.pf
	pc.mu.Unlock()
	engine := cfg.Semantic.Engine
	var recs []*Record
	var ridx []int
	for _, i := range idxs {
		s := samples[i]
		if cfg.Semantic.SubsetFraction < 1.0 &&
			flc.Uniform(strconv.FormatInt(cfg.Seed, 10), "semantic_subset", s.SampleID) >= cfg.Semantic.SubsetFraction {
			counters["semantic.not_in_subset"]++
			continue
		}
		abs := filepath.Join(repo.Root, filepath.FromSlash(s.RelativePath))
		variant, others, pkgPath, reason := packageFiles(l, pf, abs, relDir, s, cfg)
		pc.mu.Lock()
		src, ok := pc.srcs[abs]
		if !ok {
			b, err := os.ReadFile(abs)
			if err == nil && flc.Sha256Hex(b) == s.SourceSha256 {
				src = b
			}
			pc.srcs[abs] = src
		}
		pc.mu.Unlock()
		for _, policy := range cfg.Semantic.Policies {
			rec := &Record{SchemaVersion: "flc-semantic/v1", SampleID: s.SampleID, VisibilityPolicy: policy,
				Project: s.Project, PackagePath: pkgPath}
			NormalizeSlices(rec)
			switch {
			case reason != "":
				rec.Status, rec.Reason = "syntax_fallback", sp(reason)
			case src == nil:
				rec.Status, rec.Reason = "failed", sp("document_text_mismatch")
			default:
				func() {
					defer func() {
						if r := recover(); r != nil {
							*rec = Record{SchemaVersion: "flc-semantic/v1", SampleID: s.SampleID, VisibilityPolicy: policy,
								Project: s.Project, PackagePath: pkgPath, Status: "failed",
								Reason: sp(fmt.Sprintf("exception:%v", r))}
							NormalizeSlices(rec)
							if os.Getenv("GOFLC_DEBUG") != "" {
								fmt.Fprintf(os.Stderr, "panic %s: %v\n%s\n", s.SampleID, r, debug.Stack())
							}
						}
					}()
					// original_scope serves editor_snapshot only: strict_prefix must not know declarations after the
					// caret even as type names, which only the prefix snapshot check guarantees.
					if engine != "snapshot" && policy == "editor_snapshot" {
						pc.mu.Lock()
						op, ok := pc.origPkgs[variant]
						if !ok {
							op = l.checkOriginal(pkgPath, append(append([]string{}, others...), abs))
							pc.origPkgs[variant] = op
							counters["semantic.original_packages_checked"]++
						}
						if op != nil {
							if f := op.files[abs]; f != nil {
								pc.origAST[abs] = f
							}
						}
						pc.mu.Unlock()
						if analyzeOriginal(l, rec, op, abs, src, s.CaretByteOffset, s.TargetText, policy, lim) {
							return
						}
					}
					pc.mu.Lock()
					if pc.parsed[variant] == nil {
						// the whole variant (others + this file), so files analyzed later still see this one
						pc.parsed[variant] = l.ParseFiles(append(append([]string{}, others...), abs))
						counters["semantic.packages_parsed"]++
					}
					var files []*ast.File
					for _, o := range others {
						if f := pc.parsed[variant][o]; f != nil {
							files = append(files, f)
						}
					}
					orig, ok := pc.origAST[abs]
					if !ok {
						orig, _ = parser.ParseFile(l.Fset, abs, src, parser.SkipObjectResolution)
						pc.origAST[abs] = orig
					}
					pc.mu.Unlock()
					snap, suffix := snapshot(src, s.CaretByteOffset, s.TargetEndByteOffset, policy)
					rec.SyntheticSuffix = suffix
					removed := 0
					if policy == "editor_snapshot" {
						removed = s.TargetEndByteOffset - s.CaretByteOffset
					}
					analyze(l, rec, pkgPath, files, abs+"#snapshot", snap, s.CaretByteOffset, removed, orig, s.TargetText, policy, lim)
				}()
			}
			NormalizeSlices(rec)
			if len(rec.Leakage.Violations) > 0 {
				counters["semantic.leak_audit_failed"]++
				quarantineLeak(rec)
			}
			counters["semantic."+policy+"."+rec.Status]++
			if rec.AnalysisEngine != "" {
				counters["semantic."+policy+".engine."+rec.AnalysisEngine]++
			}
			if rec.Reason != nil {
				r := *rec.Reason
				if strings.HasPrefix(r, "leak_audit:") {
					r = "leak_audit"
				}
				counters["semantic."+policy+".reason."+r]++
			}
			if len(rec.Leakage.Mentioned) > 0 {
				counters["semantic."+policy+".target_identifier_overlap"]++
			}
			if rec.ExpectedType != nil {
				counters["semantic."+policy+".expected_type"]++
			}
			if len(rec.Members) > 0 {
				counters["semantic."+policy+".members"]++
			}
			recs = append(recs, rec)
			ridx = append(ridx, i)
		}
	}
	return recs, ridx
}

func strPtr(s string) *string { return &s }

func readSamples(dir string) ([]*flc.Sample, bool, error) {
	gz := false
	if _, err := os.Stat(filepath.Join(dir, "samples.jsonl")); err != nil {
		gz = true
	}
	rc, err := flc.OpenJSONL(filepath.Join(dir, "samples.jsonl"))
	if err != nil {
		return nil, false, err
	}
	defer rc.Close()
	var out []*flc.Sample
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 1<<20), 1<<28)
	for sc.Scan() {
		var s flc.Sample
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			return nil, false, err
		}
		out = append(out, &s)
	}
	return out, gz, sc.Err()
}

// packageFiles selects the files type-checked together with the snapshot of abs.
func packageFiles(l *Loader, pf *PkgFiles, abs, relDir string, s *flc.Sample, cfg *flc.Config) (variant string, others []string, pkgPath, reason string) {
	pkgPath = l.ImportPathOf(relDir)
	if pf == nil {
		return "", nil, pkgPath, "package_list_failed"
	}
	in := func(list []string) bool {
		for _, x := range list {
			if x == abs {
				return true
			}
		}
		return false
	}
	var group []string
	switch {
	case in(pf.GoFiles):
		variant, group = "lib", pf.GoFiles
	case in(pf.TestGoFiles):
		variant, group = "test", append(append([]string{}, pf.GoFiles...), pf.TestGoFiles...)
	case in(pf.XTestFiles):
		variant, group, pkgPath = "xtest", pf.XTestFiles, pkgPath+"_test"
	default:
		return "", nil, pkgPath, "build_constraint_excluded"
	}
	if len(group) > cfg.Semantic.MaxPackageFiles {
		return "", nil, pkgPath, "package_too_large"
	}
	for _, g := range group {
		if g != abs {
			others = append(others, g)
		}
	}
	return variant, others, pkgPath, ""
}

// snapshot builds the target-free text. editor_snapshot: prefix + text after the target. strict_prefix: prefix + one
// closer per bracket left open by the prefix (computed from the prefix tokens only), recorded as synthetic_suffix.
func snapshot(src []byte, caret, targetEnd int, policy string) ([]byte, *string) {
	if policy == "editor_snapshot" {
		out := make([]byte, 0, len(src)-(targetEnd-caret))
		out = append(out, src[:caret]...)
		return append(out, src[targetEnd:]...), nil
	}
	prefix := src[:caret]
	fs := token.NewFileSet()
	f := fs.AddFile("", -1, len(prefix))
	var s scanner.Scanner
	s.Init(f, prefix, func(token.Position, string) {}, 0)
	var stack []byte
	for {
		_, t, _ := s.Scan()
		if t == token.EOF {
			break
		}
		switch t {
		case token.LPAREN:
			stack = append(stack, ')')
		case token.LBRACK:
			stack = append(stack, ']')
		case token.LBRACE:
			stack = append(stack, '}')
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	var b strings.Builder
	b.WriteByte('\n')
	for i := len(stack) - 1; i >= 0; i-- {
		b.WriteByte(stack[i])
		b.WriteByte('\n')
	}
	suffix := b.String()
	out := append(append([]byte{}, prefix...), suffix...)
	return out, &suffix
}
