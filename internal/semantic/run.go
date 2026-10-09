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

	"goflc/internal/flc"
)

// Run is the semantic pass over a finished syntax run directory: it writes semantic.jsonl and rewrites samples.jsonl
// with semantic_status/semantic_reason mirrored from the editor_snapshot record. One Loader per repository run, so
// std/module imports are type-checked once; other files of a package are parsed once per package.
func Run(dir string, cfg *flc.Config, repo *flc.RepoInfo) (map[string]int, error) {
	samples, gz, err := readSamples(dir)
	if err != nil {
		return nil, err
	}
	counters := map[string]int{}
	l := NewLoader(repo.Root, repo.ModulePath, cfg.Discovery.BuildContext.GOOS, cfg.Discovery.BuildContext.GOARCH,
		cfg.Discovery.BuildContext.Tags, cfg.Semantic.UseVendor)
	lim := limits{maxScope: cfg.Semantic.MaxScopeSymbols, maxMembers: cfg.Semantic.MaxMembers, maxTypes: 6, maxTypeMembers: 10}
	out, err := flc.NewJSONLWriter(filepath.Join(dir, "semantic.jsonl"), gz)
	if err != nil {
		return nil, err
	}
	// group by package directory, keep sample order inside a group; groups in sorted order (deterministic)
	byPkg := map[string][]int{}
	for i, s := range samples {
		byPkg[s.Project] = append(byPkg[s.Project], i)
	}
	pkgs := make([]string, 0, len(byPkg))
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	status := make([]*Record, len(samples))
	for _, p := range pkgs {
		absDir := filepath.Join(repo.Root, filepath.FromSlash(p))
		pf, _ := l.ListDir(absDir)
		parsed := map[string]map[string]*ast.File{} // variant -> abs path -> file
		origAST := map[string]*ast.File{}           // original (complete) file ASTs for decl_splice
		for _, i := range byPkg[p] {
			s := samples[i]
			if cfg.Semantic.SubsetFraction < 1.0 &&
				flc.Uniform(strconv.FormatInt(cfg.Seed, 10), "semantic_subset", s.SampleID) >= cfg.Semantic.SubsetFraction {
				counters["semantic.not_in_subset"]++
				continue
			}
			abs := filepath.Join(repo.Root, filepath.FromSlash(s.RelativePath))
			variant, others, pkgPath, reason := packageFiles(l, pf, abs, p, s, cfg)
			for _, policy := range cfg.Semantic.Policies {
				rec := &Record{SchemaVersion: "flc-semantic/v1", SampleID: s.SampleID, VisibilityPolicy: policy,
					AnalysisEngine: "package_typecheck", Project: s.Project, PackagePath: pkgPath,
					Locals: []Fact{}, Parameters: []Fact{}, ReceiverMembers: []Fact{}, PackageMembers: []Fact{},
					Imports: []ImportFact{}, Members: []Fact{}, ContextTypes: []TypeContract{}, UnresolvedImports: []string{},
					SnapshotRepairs: []string{}}
				if reason != "" {
					rec.Status, rec.Reason = "syntax_fallback", sp(reason)
				} else {
					if parsed[variant] == nil {
						parsed[variant] = l.ParseFiles(others)
						counters["semantic.packages_parsed"]++
					}
					var files []*ast.File
					for _, o := range others {
						if f := parsed[variant][o]; f != nil {
							files = append(files, f)
						}
					}
					src, err := os.ReadFile(abs)
					if err != nil || flc.Sha256Hex(src) != s.SourceSha256 {
						rec.Status, rec.Reason = "failed", sp("document_text_mismatch")
					} else {
						snap, suffix := snapshot(src, s.CaretByteOffset, s.TargetEndByteOffset, policy)
						rec.SyntheticSuffix = suffix
						func() {
							defer func() {
								if r := recover(); r != nil {
									rec.Status, rec.Reason = "failed", sp(fmt.Sprintf("exception:%v", r))
									if os.Getenv("GOFLC_DEBUG") != "" {
										fmt.Fprintf(os.Stderr, "panic %s: %v\n%s\n", s.SampleID, r, debug.Stack())
									}
								}
							}()
							removed := 0
							if policy == "editor_snapshot" {
								removed = s.TargetEndByteOffset - s.CaretByteOffset
							}
							if _, ok := origAST[abs]; !ok {
								origAST[abs], _ = parser.ParseFile(l.Fset, abs, src, parser.SkipObjectResolution)
							}
							analyze(l, rec, pkgPath, files, abs+"#snapshot", snap, s.CaretByteOffset, removed, origAST[abs], s.TargetText, policy, lim)
						}()
					}
				}
				counters["semantic."+policy+"."+rec.Status]++
				if rec.Reason != nil {
					counters["semantic."+policy+".reason."+*rec.Reason]++
				}
				if len(rec.Leakage.Violations) > 0 {
					counters["semantic.leakage_violations"]++
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
				NormalizeSlices(rec)
				if err := out.Write(rec); err != nil {
					return nil, err
				}
				if policy == "editor_snapshot" || (status[i] == nil && len(cfg.Semantic.Policies) == 1) {
					status[i] = rec
				}
			}
		}
	}
	if err := out.Close(); err != nil {
		return nil, err
	}
	for k, v := range l.Stats {
		counters["semantic.loader."+k] = v
	}
	counters["semantic.loader.unresolved_import_paths"] = len(l.Unresolved)
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
	name := "samples.jsonl"
	if gz {
		// re-compress
		b, err := os.ReadFile(filepath.Join(dir, "samples.jsonl.sem-tmp"))
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
		return counters, os.Remove(filepath.Join(dir, "samples.jsonl.sem-tmp"))
	}
	return counters, os.Rename(filepath.Join(dir, "samples.jsonl.sem-tmp"), filepath.Join(dir, name))
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
