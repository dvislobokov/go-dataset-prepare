package flc

import (
	"bytes"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// DiscoveryRecord is one candidate .go path with its decision (discovery-file/v1).
type DiscoveryRecord struct {
	SchemaVersion   string  `json:"schema_version"`
	RelativePath    string  `json:"relative_path"`
	Accepted        bool    `json:"accepted"`
	SkipReason      *string `json:"skip_reason"`
	Detail          *string `json:"detail"`
	Bytes           int64   `json:"bytes"`
	Sha256          *string `json:"sha256"`
	DuplicateOf     *string `json:"duplicate_of"`
	IsTest          bool    `json:"is_test"`
	Project         string  `json:"project"`
	PackageName     *string `json:"package_name"`
	BuildConstraint *string `json:"build_constraint"`
	BuildMatch      *bool   `json:"build_match"`
	Cgo             bool    `json:"cgo"`
}

// AcceptedFile is an accepted source file ready for extraction.
type AcceptedFile struct {
	Rel             string
	Src             *Source
	IsTest          bool
	Project         string // package directory relative to the repository root ("." for the root)
	PackageName     string
	BuildConstraint *string
	BuildMatch      bool
	Cgo             bool
}

// RepoInfo holds repository-level facts.
type RepoInfo struct {
	Root           string
	RepositoryID   string
	Revision       *string
	License        *string // effective SPDX id or nil
	LicenseReason  string
	LicenseAllowed bool
	ModulePath     *string // from the root go.mod (parsed as text; the go command is never run)
	GoVersion      *string
}

type Discoverer struct {
	cfg        *Config
	include    *GlobMatcher
	exclude    *GlobMatcher
	generated  *GlobMatcher
	vendored   *GlobMatcher
	testdata   *GlobMatcher
	tests      *GlobMatcher
	markers    []*regexp.Regexp
	secrets    []*regexp.Regexp
	buildCtx   build.Context
	seenHashes map[string]string
}

func NewDiscoverer(cfg *Config) *Discoverer {
	d := &Discoverer{
		cfg:        cfg,
		include:    NewGlobMatcher(cfg.Discovery.Include),
		exclude:    NewGlobMatcher(cfg.Discovery.Exclude),
		generated:  NewGlobMatcher(cfg.Discovery.GeneratedPatterns),
		vendored:   NewGlobMatcher(cfg.Discovery.VendoredPatterns),
		testdata:   NewGlobMatcher(cfg.Discovery.TestdataPatterns),
		tests:      NewGlobMatcher(cfg.Discovery.TestPatterns),
		seenHashes: map[string]string{},
	}
	for _, m := range cfg.Discovery.GeneratedMarkers {
		d.markers = append(d.markers, regexp.MustCompile(m))
	}
	for _, p := range cfg.Secrets.Patterns {
		d.secrets = append(d.secrets, regexp.MustCompile(p))
	}
	d.buildCtx = BuildContext(cfg.Discovery.BuildContext)
	return d
}

// BuildContext returns a go/build context that never shells out: no GOPATH/module lookups are performed by MatchFile.
func BuildContext(bc BuildContextConfig) build.Context {
	ctx := build.Default
	ctx.GOOS = bc.GOOS
	ctx.GOARCH = bc.GOARCH
	ctx.CgoEnabled = bc.Cgo
	ctx.BuildTags = append([]string{}, bc.Tags...)
	return ctx
}

func strp(s string) *string { return &s }

// Discover walks the repository (symlinks are not followed; .git is pruned) in sorted order.
func (d *Discoverer) Discover(root string, repo *RepoInfo, onAccepted func(*AcceptedFile), onRecord func(*DiscoveryRecord)) error {
	var rels []string
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are ignored (recorded nowhere: not a candidate path)
		}
		if e.IsDir() {
			if e.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if e.Type()&fs.ModeSymlink != 0 || !e.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.include.Match(rel) {
			rels = append(rels, rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(rels) // ordinal byte order
	for _, rel := range rels {
		rec, acc := d.decide(root, rel, repo)
		onRecord(rec)
		if acc != nil {
			onAccepted(acc)
		}
	}
	return nil
}

func goIgnoredDir(rel string) bool {
	parts := strings.Split(rel, "/")
	for _, p := range parts[:len(parts)-1] {
		if strings.HasPrefix(p, "_") || strings.HasPrefix(p, ".") {
			return true
		}
	}
	return false
}

func (d *Discoverer) decide(root, rel string, repo *RepoInfo) (*DiscoveryRecord, *AcceptedFile) {
	dir := path.Dir(rel)
	rec := &DiscoveryRecord{SchemaVersion: "discovery-file/v1", RelativePath: rel, Project: dir,
		IsTest: d.tests.Match(rel)}
	skip := func(reason string, detail string) (*DiscoveryRecord, *AcceptedFile) {
		rec.SkipReason = strp(reason)
		if detail != "" {
			rec.Detail = strp(detail)
		}
		return rec, nil
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	st, err := os.Lstat(full)
	if err != nil {
		return skip("unreadable", err.Error())
	}
	rec.Bytes = st.Size()
	switch {
	case d.exclude.Match(rel):
		return skip("excluded_path", "")
	case d.cfg.Discovery.SkipGoIgnoredDirs && goIgnoredDir(rel):
		return skip("go_ignored_dir", "directory starts with '_' or '.' (ignored by the go tool)")
	case d.testdata.Match(rel):
		return skip("testdata_path", "")
	case d.vendored.Match(rel):
		return skip("vendored_path", "")
	case st.Size() > d.cfg.Discovery.MaxFileBytes:
		return skip("too_large", "")
	case d.generated.Match(rel):
		return skip("generated_path", "")
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return skip("unreadable", err.Error())
	}
	sha := Sha256Hex(b)
	rec.Sha256 = strp(sha)
	src, err := DecodeSource(b)
	if err != nil {
		return skip(err.Error(), "")
	}
	if len(bytes.TrimSpace(src.Text())) == 0 {
		return skip("empty", "")
	}
	// Package clause + header comments only: cheap, and tells us whether this is Go at all (templates are not).
	fset := token.NewFileSet()
	hdr, perr := parser.ParseFile(fset, rel, b, parser.PackageClauseOnly|parser.ParseComments)
	if perr != nil || hdr == nil || hdr.Name == nil {
		return skip("invalid_package_clause", "")
	}
	rec.PackageName = strp(hdr.Name.Name)
	if ast.IsGenerated(hdr) {
		return skip("generated_marker", "// Code generated ... DO NOT EDIT.")
	}
	head := b
	if len(head) > 4000 {
		head = head[:4000]
	}
	for _, m := range d.markers {
		if m.Match(head) {
			return skip("generated_marker", "loose: "+m.String())
		}
	}
	// Build constraints: //go:build line (+ legacy // +build) and GOOS/GOARCH file name suffixes, evaluated with a
	// fixed context; go/build.MatchFile only reads the file header (no go command, no cgo processing).
	if expr := buildConstraintOf(hdr); expr != "" {
		rec.BuildConstraint = strp(expr)
	}
	ctx := d.buildCtx
	ctx.OpenFile = func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	match, merr := ctx.MatchFile(filepath.Join(root, dir), path.Base(rel))
	if merr != nil {
		match = false
	}
	rec.Cgo = importsC(b)
	if rec.Cgo && !d.cfg.Discovery.BuildContext.Cgo {
		match = false // cgo files are not part of the package when cgo is disabled (go/build semantics)
	}
	rec.BuildMatch = &match
	if !match && d.cfg.Discovery.BuildExcludedPolicy == "skip" {
		return skip("build_excluded", "")
	}
	maxLine := 0
	for i := range src.LineStarts {
		if n := src.LineContentEnd[i] - src.LineStarts[i]; n > maxLine {
			maxLine = n
		}
	}
	if maxLine > d.cfg.Discovery.MaxFileLineChars {
		return skip("long_lines", "")
	}
	if !repo.LicenseAllowed {
		return skip("license_not_allowed", repo.LicenseReason)
	}
	for _, rx := range d.secrets {
		if rx.Match(b) {
			return skip("secret_detected", rx.String())
		}
	}
	if first, ok := d.seenHashes[sha]; ok {
		rec.DuplicateOf = strp(first)
		return skip("exact_duplicate", "")
	}
	d.seenHashes[sha] = rel
	rec.Accepted = true
	return rec, &AcceptedFile{Rel: rel, Src: src, IsTest: rec.IsTest, Project: dir, PackageName: hdr.Name.Name,
		BuildConstraint: rec.BuildConstraint, BuildMatch: match, Cgo: rec.Cgo}
}

func buildConstraintOf(f *ast.File) string {
	var plus []string
	for _, cg := range f.Comments {
		if cg.Pos() > f.Package {
			break
		}
		for _, c := range cg.List {
			if constraint.IsGoBuild(c.Text) {
				if e, err := constraint.Parse(c.Text); err == nil {
					return e.String()
				}
				return strings.TrimSpace(strings.TrimPrefix(c.Text, "//go:build"))
			}
			if constraint.IsPlusBuild(c.Text) {
				plus = append(plus, strings.TrimSpace(strings.TrimPrefix(c.Text, "// +build")))
			}
		}
	}
	if len(plus) > 0 {
		return "+build " + strings.Join(plus, " ; ")
	}
	return ""
}

var importCRe = regexp.MustCompile(`(?m)^\s*import\s+(?:\(\s*)?"C"`)

func importsC(b []byte) bool { return importCRe.Match(b) }

// ParseGoMod reads the module path and go version from go.mod as text (the go command is never invoked).
func ParseGoMod(root string) (modulePath, goVersion *string) {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, nil
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if strings.HasPrefix(line, "module ") && modulePath == nil {
			m := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module ")), `"`)
			modulePath = &m
		}
		if strings.HasPrefix(line, "go ") && goVersion == nil {
			v := strings.TrimSpace(strings.TrimPrefix(line, "go "))
			goVersion = &v
		}
	}
	return
}
