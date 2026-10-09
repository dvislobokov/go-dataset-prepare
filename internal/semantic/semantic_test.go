package semantic

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goflc/internal/flc"
)

const caretMark = "▮"

var storeSrc = `package store

import "context"

type User struct {
	ID   int
	Name string
	hidden bool
}

type Store struct{ users map[int]*User }

func New() *Store { return &Store{users: map[int]*User{}} }

func (s *Store) GetUser(ctx context.Context, id int) (*User, error) { return s.users[id], nil }

func (s *Store) unexportedHelper() {}
`

// at writes a module with the given files (one containing ▮), and analyzes the caret for policy. The target is the
// rest of the caret's physical line without trailing whitespace, exactly as the extractor defines it.
func at(t *testing.T, files map[string]string, policy string) *Record {
	t.Helper()
	dir := t.TempDir()
	var rel string
	var caret, te int
	var src []byte
	for name, content := range files {
		if i := strings.Index(content, caretMark); i >= 0 {
			rel = name
			content = content[:i] + content[i+len(caretMark):]
			caret = i
			e := caret + strings.IndexByte(content[caret:]+"\n", '\n')
			te = flc.TrimRightHSpace([]byte(content), caret, e)
			src = []byte(content)
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	if rel == "" {
		t.Fatal("no caret marker")
	}
	mod := "example.com/m"
	l := NewLoader(dir, &mod, "linux", "amd64", nil, true)
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	pkgDir := filepath.Dir(abs)
	pf, err := l.ListDir(pkgDir)
	if err != nil && pf == nil {
		t.Fatal(err)
	}
	var others []string
	for _, f := range append(pf.GoFiles, pf.TestGoFiles...) {
		if f != abs {
			others = append(others, f)
		}
	}
	parsed := l.ParseFiles(others)
	var files2 []*ast.File
	for _, o := range others {
		files2 = append(files2, parsed[o])
	}
	relDir, _ := filepath.Rel(dir, pkgDir)
	rec := &Record{SchemaVersion: "flc-semantic/v1", VisibilityPolicy: policy, Locals: []Fact{}, Parameters: []Fact{},
		ReceiverMembers: []Fact{}, PackageMembers: []Fact{}, Members: []Fact{}, ContextTypes: []TypeContract{}}
	snap, suffix := snapshot(src, caret, te, policy)
	rec.SyntheticSuffix = suffix
	removed := 0
	if policy == "editor_snapshot" {
		removed = te - caret
	}
	orig, _ := parser.ParseFile(l.Fset, abs, src, parser.SkipObjectResolution)
	analyze(l, rec, l.ImportPathOf(filepath.ToSlash(relDir)), files2, abs+"#snapshot", snap, caret, removed, orig,
		string(src[caret:te]), policy, limits{maxScope: 100, maxMembers: 100, maxTypes: 6, maxTypeMembers: 10})
	return rec
}

func names(fs []Fact) map[string]string {
	m := map[string]string{}
	for _, f := range fs {
		t := ""
		if f.Type != nil {
			t = *f.Type
		}
		if f.Signature != nil {
			t = *f.Signature
		}
		m[f.Name] = t
	}
	return m
}

func allFactNames(r *Record) map[string]bool {
	m := map[string]bool{}
	for _, list := range [][]Fact{r.Locals, r.Parameters, r.ReceiverMembers, r.PackageMembers, r.Members} {
		for _, f := range list {
			m[f.Name] = true
		}
	}
	for _, ct := range r.ContextTypes {
		m[ct.Name] = true
		for _, f := range ct.Members {
			m[f.Name] = true
		}
	}
	return m
}

var policies = []string{"editor_snapshot", "strict_prefix"}

func TestLocalDeclaredInTargetNeverAppears(t *testing.T) {
	for _, p := range policies {
		r := at(t, map[string]string{"go.mod": "module example.com/m\n", "a.go": `package a

func f(a int) int {
	b := a + 1
	▮secretLocalName := b * 2
	return secretLocalName
}
`}, p)
		loc := names(r.Locals)
		if _, ok := loc["secretLocalName"]; ok {
			t.Fatalf("%s: local declared in target leaked: %v", p, loc)
		}
		if loc["b"] != "int" || names(r.Parameters)["a"] != "int" {
			t.Fatalf("%s: wrong scope facts locals=%v params=%v", p, loc, names(r.Parameters))
		}
		if len(r.Leakage.Violations) != 0 {
			t.Fatalf("%s: violations %v", p, r.Leakage.Violations)
		}
	}
}

func TestInvocationOnlyInTargetNeverAppears(t *testing.T) {
	for _, p := range policies {
		r := at(t, map[string]string{"go.mod": "module example.com/m\n", "a.go": `package a

type T struct{ n int }

func (t *T) Known() int { return t.n }

func g(x *T) {
	x.▮undefinedHelperOnlyInTarget(42)
}
`}, p)
		if allFactNames(r)["undefinedHelperOnlyInTarget"] {
			t.Fatalf("%s: invocation from hidden target copied into facts", p)
		}
		m := names(r.Members)
		if _, ok := m["Known"]; !ok {
			t.Fatalf("%s: receiver members missing Known: %v (status %s %v)", p, m, r.Status, r.Reason)
		}
		if *r.ReceiverType != "*T" {
			t.Fatalf("%s: receiver type %v", p, *r.ReceiverType)
		}
	}
}

func TestVarTypeAndCrossPackageMembers(t *testing.T) {
	r := at(t, map[string]string{"go.mod": "module example.com/m\n", "store/store.go": storeSrc, "app/app.go": `package app

import (
	"context"

	"example.com/m/store"
)

func run(ctx context.Context) {
	u := store.New()
	user, err := u.▮GetUser(ctx, 1)
	_, _ = user, err
}
`}, "editor_snapshot")
	if r.Status != "resolved" {
		t.Fatalf("status %s %v", r.Status, r.Reason)
	}
	if names(r.Locals)["u"] != "*store.Store" {
		t.Fatalf("var type not recognized: %v", names(r.Locals))
	}
	m := names(r.Members)
	if _, ok := m["GetUser"]; !ok {
		t.Fatalf("members %v", m)
	}
	if _, ok := m["unexportedHelper"]; ok {
		t.Fatal("unexported member of another package listed")
	}
	if _, ok := names(r.Locals)["user"]; ok {
		t.Fatal("variable being declared on the caret line is in scope")
	}
	// GetUser legitimately exists in another package: coverage, not leakage
	if len(r.Leakage.Violations) != 0 || !contains(r.Leakage.Mentioned, "GetUser") {
		t.Fatalf("leakage report %+v", r.Leakage)
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func TestPackageSelectorAndExpectedTypes(t *testing.T) {
	r := at(t, map[string]string{"go.mod": "module example.com/m\n", "a.go": `package a

import "strings"

func f(s string) (int, error) {
	return strings.▮Count(s, "x"), nil
}
`}, "editor_snapshot")
	if *r.ReceiverKind != "package" || *r.ReceiverType != "strings" {
		t.Fatalf("receiver %v %v", r.ReceiverKind, r.ReceiverType)
	}
	if _, ok := names(r.Members)["Count"]; !ok {
		t.Fatal("strings members missing Count")
	}
	if !contains(r.SnapshotRepairs, "phantom_selector") {
		t.Fatalf("repairs %v", r.SnapshotRepairs)
	}

	r = at(t, map[string]string{"go.mod": "module example.com/m\n", "a.go": `package a

func f(s string) (int, error) {
	return 1, ▮nil
}
`}, "editor_snapshot")
	if r.ExpectedType == nil || *r.ExpectedType != "error" || *r.ExpectedTypeSource != "return" {
		t.Fatalf("expected type %v", r.ExpectedType)
	}

	r = at(t, map[string]string{"go.mod": "module example.com/m\n", "a.go": `package a

import "time"

func take(n int, d time.Duration, rest ...string) {}

func f() {
	take(1, ▮time.Second)
}
`}, "editor_snapshot")
	if r.ExpectedType == nil || *r.ExpectedType != "time.Duration" || *r.ArgumentIndex != 1 {
		t.Fatalf("argument expected type %v idx %v sig %v", r.ExpectedType, r.ArgumentIndex, r.CallSignature)
	}
}

func TestStrictPrefixHidesLaterDeclarations(t *testing.T) {
	files := map[string]string{"go.mod": "module example.com/m\n", "a.go": `package a

func f() int {
	x := 1
	return ▮x
}

func laterFunc() int { return 2 }
`}
	ed := at(t, files, "editor_snapshot")
	st := at(t, files, "strict_prefix")
	if _, ok := names(ed.PackageMembers)["laterFunc"]; !ok {
		t.Fatalf("editor_snapshot should see laterFunc: %v", names(ed.PackageMembers))
	}
	if _, ok := names(st.PackageMembers)["laterFunc"]; ok {
		t.Fatal("strict_prefix saw a declaration after the caret")
	}
	if st.SyntheticSuffix == nil || *st.SyntheticSuffix != "\n}\n" {
		t.Fatalf("synthetic suffix %q", *st.SyntheticSuffix)
	}
	if names(st.Locals)["x"] != "int" || *st.ExpectedType != "int" {
		t.Fatal("strict_prefix lost prefix facts")
	}
}

func TestEditorSnapshotKeepsLaterDeclsDespiteBrokenLine(t *testing.T) {
	// removing "{" with the target unbalances the function; decl_splice keeps the following declarations
	r := at(t, map[string]string{"go.mod": "module example.com/m\n", "a.go": `package a

func f(err error) int {
	if ▮err != nil {
		return 1
	}
	return 0
}

func after1() {}

type After2 struct{ A int }
`}, "editor_snapshot")
	pm := names(r.PackageMembers)
	if _, ok := pm["after1"]; !ok {
		t.Fatalf("lost later declarations: %v", pm)
	}
	if _, ok := pm["After2"]; !ok {
		t.Fatalf("lost later type: %v", pm)
	}
	if names(r.Parameters)["err"] != "error" {
		t.Fatal("params lost")
	}
}

func TestExternalImportsArePartial(t *testing.T) {
	r := at(t, map[string]string{"go.mod": "module example.com/m\n", "a.go": `package a

import "github.com/not/downloaded"

func f() {
	downloaded.▮Do()
}
`}, "editor_snapshot")
	if r.Status != "partially_resolved" || r.Reason == nil || *r.Reason != "unresolved_import" {
		t.Fatalf("status %s %v", r.Status, r.Reason)
	}
	if !contains(r.UnresolvedImports, "github.com/not/downloaded") {
		t.Fatalf("unresolved %v", r.UnresolvedImports)
	}
	r = at(t, map[string]string{"go.mod": "module example.com/m\n", "a.go": `package a

import "github.com/not/downloaded"

func f(n int) int {
	_ = downloaded.X
	return ▮n
}
`}, "editor_snapshot")
	if r.Status != "partially_resolved" || *r.Reason != "external_imports_unresolved" {
		t.Fatalf("status %s %v", r.Status, *r.Reason)
	}
	if names(r.Parameters)["n"] != "int" {
		t.Fatal("facts should still be emitted")
	}
}

func TestMalformedSourceDoesNotCrash(t *testing.T) {
	for _, p := range policies {
		r := at(t, map[string]string{"go.mod": "module example.com/m\n", "a.go": "package a\n\nfunc f( {\n\tx := ▮[[[\n}}}\nfunc\n"}, p)
		if r.Status == "" {
			t.Fatal("no status")
		}
	}
}

// End-to-end: extract with semantic mode on the fixture repository, validate (incl. leakage audit), determinism.
func TestEndToEndFixtureSemantic(t *testing.T) {
	repo, _ := filepath.Abs("../../fixtures/basic")
	cfg := flc.DefaultConfig()
	cfg.Semantic.Mode = "best_effort"
	cp := filepath.Join(t.TempDir(), "c.json")
	b, _ := json.Marshal(cfg)
	os.WriteFile(cp, b, 0o644)
	var outs []string
	for i := 0; i < 2; i++ {
		out := filepath.Join(t.TempDir(), "out")
		if _, err := flc.Run(flc.RunOptions{Repo: repo, Out: out, ConfigPath: cp, RepositoryID: "fixture/basic",
			Workers: 1 + 3*i, SemanticHook: Run}); err != nil {
			t.Fatal(err)
		}
		rep, err := flc.Validate(out, repo)
		if err != nil {
			t.Fatal(err)
		}
		if !rep.OK || rep.Semantic == 0 {
			t.Fatalf("validation %v %v semantic=%d", rep.Failures, rep.Examples, rep.Semantic)
		}
		outs = append(outs, out)
	}
	for _, f := range []string{"samples.jsonl", "semantic.jsonl", "summary.json"} {
		a, _ := os.ReadFile(filepath.Join(outs[0], f))
		c, _ := os.ReadFile(filepath.Join(outs[1], f))
		if !bytes.Equal(a, c) {
			t.Errorf("%s not deterministic", f)
		}
	}
	sem, _ := os.ReadFile(filepath.Join(outs[0], "semantic.jsonl"))
	if !bytes.Contains(sem, []byte(`"reason":"build_constraint_excluded"`)) {
		t.Error("windows-only file should fall back with build_constraint_excluded")
	}
	if !bytes.Contains(sem, []byte(`"status":"resolved"`)) {
		t.Error("no resolved records")
	}
}

// atOriginal analyzes the caret with the cached original-package engine (editor_snapshot). ok=false when the caret
// is not eligible (outside a function body).
func atOriginal(t *testing.T, files map[string]string) (*Record, bool) {
	t.Helper()
	dir := t.TempDir()
	var rel string
	var caret, te int
	var src []byte
	for name, content := range files {
		if i := strings.Index(content, caretMark); i >= 0 {
			rel = name
			content = content[:i] + content[i+len(caretMark):]
			caret = i
			e := caret + strings.IndexByte(content[caret:]+"\n", '\n')
			te = flc.TrimRightHSpace([]byte(content), caret, e)
			src = []byte(content)
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	mod := "example.com/m"
	l := NewLoader(dir, &mod, "linux", "amd64", nil, true)
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	pf, _ := l.ListDir(filepath.Dir(abs))
	relDir, _ := filepath.Rel(dir, filepath.Dir(abs))
	op := l.checkOriginal(l.ImportPathOf(filepath.ToSlash(relDir)), append(append([]string{}, pf.GoFiles...), pf.TestGoFiles...))
	rec := &Record{SchemaVersion: "flc-semantic/v1", VisibilityPolicy: "editor_snapshot"}
	NormalizeSlices(rec)
	ok := analyzeOriginal(l, rec, op, abs, src, caret, string(src[caret:te]), "editor_snapshot",
		limits{maxScope: 100, maxMembers: 100, maxTypes: 6, maxTypeMembers: 10})
	return rec, ok
}

func TestOriginalEngineLeakageRules(t *testing.T) {
	mod := "module example.com/m\n"
	// local declared in the target
	r, ok := atOriginal(t, map[string]string{"go.mod": mod, "a.go": "package a\n\nfunc f(a int) int {\n\tb := a + 1\n\t▮secretLocalName := b * 2\n\treturn secretLocalName\n}\n"})
	if !ok || r.AnalysisEngine != "original_scope" {
		t.Fatal("caret in a function body must use original_scope")
	}
	if _, bad := names(r.Locals)["secretLocalName"]; bad || names(r.Locals)["b"] != "int" {
		t.Fatalf("locals %v", names(r.Locals))
	}
	// declarator being typed is never in scope (both engines)
	files := map[string]string{"go.mod": mod, "a.go": "package a\n\ntype Client struct{ N int }\n\nfunc f() {\n\tclient1 := &Cl▮ient{N: 1}\n\t_ = client1\n}\n"}
	r, _ = atOriginal(t, files)
	if _, bad := names(r.Locals)["client1"]; bad {
		t.Fatal("original engine: declarator being typed is visible")
	}
	if _, bad := names(at(t, files, "editor_snapshot").Locals)["client1"]; bad {
		t.Fatal("snapshot engine: declarator being typed is visible")
	}
	// invocation only in the target
	r, _ = atOriginal(t, map[string]string{"go.mod": mod, "a.go": "package a\n\ntype T struct{}\n\nfunc (T) Known() {}\n\nfunc g(x T) {\n\tx.▮Known()\n\tundefinedOnlyHere()\n}\n"})
	if _, ok := names(r.Members)["Known"]; !ok || len(r.Leakage.Violations) != 0 {
		t.Fatalf("members %v leakage %+v", names(r.Members), r.Leakage)
	}
}

func TestOriginalEngineGenericAndBuiltinCallsDoNotLeakInstantiation(t *testing.T) {
	mod := "module example.com/m\n"
	// the generic callee is instantiated from the hidden argument on the full file: only the declared signature
	r, ok := atOriginal(t, map[string]string{"go.mod": mod, "a.go": `package a

func Map[T, U any](xs []T, f func(T) U) []U { return nil }

func g(xs []int) {
	_ = Map(xs, ▮func(x int) string { return "" })
}
`})
	if !ok {
		t.Fatal("not eligible")
	}
	if r.ExpectedType != nil {
		t.Fatalf("expected type leaked from target instantiation: %s", *r.ExpectedType)
	}
	if r.CallSignature == nil || !strings.Contains(*r.CallSignature, "U") || strings.Contains(*r.CallSignature, "string") {
		t.Fatalf("call signature must be the declared generic one, got %v", r.CallSignature)
	}
	// builtin: len(...)'s recorded signature is derived from its argument
	r, _ = atOriginal(t, map[string]string{"go.mod": mod, "a.go": "package a\n\ntype Secret []int\n\nfunc g(s Secret) int {\n\treturn len(▮s)\n}\n"})
	if r.ExpectedType != nil && *r.ExpectedType == "Secret" {
		t.Fatal("builtin argument type leaked")
	}
	if r.CallSignature != nil {
		t.Fatalf("builtin call signature emitted: %s", *r.CallSignature)
	}
}

func TestOriginalEngineNotUsedOutsideBodies(t *testing.T) {
	_, ok := atOriginal(t, map[string]string{"go.mod": "module example.com/m\n", "a.go": "package a\n\ntype T struct {\n\t▮Name string\n}\n"})
	if ok {
		t.Fatal("package-level caret must fall back to the snapshot engine")
	}
}

func TestQuarantineLeak(t *testing.T) {
	r := &Record{SchemaVersion: "flc-semantic/v1", SampleID: "x", VisibilityPolicy: "editor_snapshot", Status: "resolved",
		Locals: []Fact{{Name: "leaky", Kind: "local"}}, Leakage: Leakage{TargetIdentifiers: []string{"leaky"},
			Violations: []string{"name_only_in_target:leaky"}}}
	quarantineLeak(r)
	if r.Status != "failed" || *r.Reason != "leak_audit:leaky" || len(r.Locals) != 0 || len(r.Leakage.Violations) != 0 {
		t.Fatalf("%+v", r)
	}
}
