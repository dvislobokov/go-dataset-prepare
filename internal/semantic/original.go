package semantic

import (
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"sort"
	"strings"
)

// origPkg is the type-checked ORIGINAL package (one build variant: lib, lib+tests, or x_test), computed once and
// shared by every caret of that package — the Go analogue of binding against the cached Roslyn compilation.
type origPkg struct {
	pkg        *types.Package
	info       *types.Info
	files      map[string]*ast.File // absolute path -> full AST (bodies kept)
	identCount map[string]int       // identifier occurrences over all files of the variant
	nerr       int
}

func (l *Loader) checkOriginal(pkgPath string, paths []string) *origPkg {
	sorted := append([]string{}, paths...)
	sort.Strings(sorted)
	op := &origPkg{files: map[string]*ast.File{}, identCount: map[string]int{}}
	var files []*ast.File
	for _, p := range sorted {
		f, _ := parser.ParseFile(l.Fset, p, nil, parser.SkipObjectResolution)
		if f == nil {
			continue
		}
		op.files[p] = f
		files = append(files, f)
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				op.identCount[id.Name]++
			}
			return true
		})
	}
	op.info = &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{},
		Uses: map[*ast.Ident]types.Object{}, Scopes: map[ast.Node]*types.Scope{}}
	conf := types.Config{Importer: l, Error: func(error) { op.nerr++ }, FakeImportC: true}
	op.pkg, _ = conf.Check(pkgPath, l.Fset, files, op.info)
	return op
}

// analyzeOriginal reads facts at the caret from the cached original package. It applies only when the caret lies
// inside a function body, where everything the facts depend on is text before the caret or package-level
// declarations that the edit cannot change:
//
//   - locals/parameters: go/types scopes start after the declaring statement/signature, so a symbol declared on the
//     edited line (i.e. in the hidden target) is never visible at the caret position (LookupParent + pos < caret);
//   - receiver/selector types and call signatures come from expressions that end before the caret;
//   - a generic callee's (or builtin's) recorded signature is instantiated from all arguments, including those in
//     the hidden target: only the declared generic signature is emitted, with no expected type;
//   - the leakage audit uses identifier counts of the package minus the target's identifiers (= the snapshot's).
//
// Used for editor_snapshot only (strict_prefix is always checked on its prefix snapshot).
// Returns false when the caret is not eligible; rec is untouched in that case.
func analyzeOriginal(l *Loader, rec *Record, op *origPkg, abs string, src []byte, caret int, targetText, policy string,
	lim limits) bool {
	if op == nil || op.pkg == nil {
		return false
	}
	file := op.files[abs]
	if file == nil {
		return false
	}
	tf := l.Fset.File(file.FileStart)
	if tf == nil || caret > tf.Size() || tf.Size() != len(src) {
		return false
	}
	pos := tf.Pos(caret)
	if !inFuncBody(file, pos) {
		return false
	}
	tc := identCounts(targetText)
	present := func(n string) bool { return op.identCount[n]-tc[n] > 0 }
	r2 := *rec
	v := &view{pkg: op.pkg, info: op.info, file: file, tf: tf, text: src, original: true, identPresent: present,
		strictCut: policy == "strict_prefix"}
	extractFacts(l, &r2, v, caret, targetText, policy, lim)
	r2.AnalysisEngine = "original_scope"
	r2.TypeErrors = op.nerr
	*rec = r2
	return true
}

// inFuncBody reports whether pos is inside the body of a function declaration or of a function literal anywhere in the
// file — including literals in package-level initializers (`var _ = Describe("x", func() { ... })`, handler tables),
// which hold most of the code of Ginkgo-style test files.
func inFuncBody(file *ast.File, pos token.Pos) bool {
	found := false
	for _, d := range file.Decls {
		if d.Pos() > pos || pos > d.End() {
			continue
		}
		ast.Inspect(d, func(n ast.Node) bool {
			if found || n == nil || n.Pos() > pos || pos > n.End() {
				return false
			}
			switch x := n.(type) {
			case *ast.FuncDecl:
				if x.Body != nil && x.Body.Lbrace < pos && pos <= x.Body.Rbrace {
					found = true
				}
			case *ast.FuncLit:
				if x.Body != nil && x.Body.Lbrace < pos && pos <= x.Body.Rbrace {
					found = true
				}
			}
			return !found
		})
	}
	return found
}

func identCounts(s string) map[string]int {
	out := map[string]int{}
	fs := token.NewFileSet()
	f := fs.AddFile("", -1, len(s))
	var sc scanner.Scanner
	sc.Init(f, []byte(s), func(token.Position, string) {}, 0)
	for {
		_, t, lit := sc.Scan()
		if t == token.EOF {
			break
		}
		if t == token.IDENT {
			out[lit]++
		}
	}
	return out
}

// quarantineLeak turns a record whose facts failed the leakage audit into a fact-free failed record
// (reason leak_audit:<names>), so one bad caret never fails the repository.
func quarantineLeak(rec *Record) {
	names := make([]string, 0, len(rec.Leakage.Violations))
	for _, v := range rec.Leakage.Violations {
		if i := strings.LastIndexByte(v, ':'); i >= 0 {
			names = append(names, v[i+1:])
		} else {
			names = append(names, v)
		}
	}
	*rec = Record{SchemaVersion: rec.SchemaVersion, SampleID: rec.SampleID, VisibilityPolicy: rec.VisibilityPolicy,
		AnalysisEngine: rec.AnalysisEngine, Status: "failed", Reason: sp("leak_audit:" + strings.Join(names, ",")),
		Project: rec.Project, PackagePath: rec.PackagePath, SyntheticSuffix: rec.SyntheticSuffix,
		Leakage: Leakage{TargetIdentifiers: rec.Leakage.TargetIdentifiers}}
	NormalizeSlices(rec)
}
