package semantic

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"sort"
	"strings"
)

// Fact is one symbol usable at the caret (flc-semantic/v1 fact shape shared with the C# pipeline).
type Fact struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Type      *string `json:"type"`
	Signature *string `json:"signature,omitempty"`
	Exported  *bool   `json:"exported,omitempty"`
	obj       types.Object
}

type ImportFact struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Resolved bool   `json:"resolved"`
}

type TypeContract struct {
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Source       string `json:"source"`
	Members      []Fact `json:"members"`
	TotalMembers int    `json:"total_members"`
	obj          types.Object
}

type Leakage struct {
	TargetIdentifiers []string `json:"target_identifiers"`
	Mentioned         []string `json:"mentioned"`
	Violations        []string `json:"violations"`
}

// Record is flc-semantic/v1 (Go profile), keyed by (sample_id, visibility_policy).
type Record struct {
	SchemaVersion      string         `json:"schema_version"`
	SampleID           string         `json:"sample_id"`
	VisibilityPolicy   string         `json:"visibility_policy"`
	AnalysisEngine     string         `json:"analysis_engine"`
	Status             string         `json:"status"`
	Reason             *string        `json:"reason"`
	Project            string         `json:"project"`
	PackagePath        string         `json:"package_path"`
	EnclosingSymbol    *string        `json:"enclosing_symbol"`
	EnclosingKind      *string        `json:"enclosing_kind"`
	EnclosingType      *string        `json:"enclosing_type"`
	ReturnType         *string        `json:"return_type"`
	ExpectedType       *string        `json:"expected_type"`
	ExpectedTypeSource *string        `json:"expected_type_source"`
	Locals             []Fact         `json:"locals"`
	Parameters         []Fact         `json:"parameters"`
	ReceiverMembers    []Fact         `json:"receiver_members"`
	PackageMembers     []Fact         `json:"package_members"`
	Imports            []ImportFact   `json:"imports"`
	ReceiverType       *string        `json:"receiver_type"`
	ReceiverKind       *string        `json:"receiver_kind"`
	Members            []Fact         `json:"members"`
	CallName           *string        `json:"call_name"`
	CallSignature      *string        `json:"call_signature"`
	ArgumentIndex      *int           `json:"argument_index"`
	ParameterName      *string        `json:"parameter_name"`
	ContextTypes       []TypeContract `json:"context_types"`
	UnresolvedImports  []string       `json:"unresolved_imports"`
	TypeErrors         int            `json:"type_errors"`
	SyntheticSuffix    *string        `json:"synthetic_suffix"`
	SnapshotRepairs    []string       `json:"snapshot_repairs"`
	Truncated          bool           `json:"truncated"`
	Leakage            Leakage        `json:"leakage"`
	Prompt             string         `json:"prompt"`
}

func sp(s string) *string { return &s }

type limits struct {
	maxScope, maxMembers, maxTypes, maxTypeMembers int
}

// analyze type-checks the package with the snapshot file substituted and extracts facts at caret.
//
//	snapshot  target-free bytes of the file (prefix + rest-of-file for editor_snapshot; prefix + closers for strict_prefix)
//	caret     byte offset of the caret (identical in the original and the snapshot)
//	targetLen bytes removed at the caret (editor_snapshot) — used to map snapshot offsets back for the leakage audit
func analyze(l *Loader, rec *Record, pkgPath string, others []*ast.File, filename string, snapshot []byte, caret int,
	removed int, orig *ast.File, targetText string, policy string, lim limits) {
	file, tf, repairs := buildSnapshotFile(l, filename, snapshot, caret, removed, orig, policy)
	rec.SnapshotRepairs = repairs
	if file == nil || tf == nil {
		rec.Status, rec.Reason = "failed", sp("parse_error")
		return
	}
	if file.Name == nil || file.Name.Name == "" || file.Name.Name == "_" {
		rec.Status, rec.Reason = "syntax_fallback", sp("no_package_clause")
		return
	}
	if caret > tf.Size() {
		rec.Status, rec.Reason = "failed", sp("caret_out_of_range")
		return
	}
	files := append([]*ast.File{file}, others...)
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{},
		Uses: map[*ast.Ident]types.Object{}, Scopes: map[ast.Node]*types.Scope{}}
	nerr := 0
	conf := types.Config{Importer: l, Error: func(error) { nerr++ }, FakeImportC: true}
	pkg, _ := conf.Check(pkgPath, l.Fset, files, info)
	rec.TypeErrors = nerr
	if pkg == nil {
		rec.Status, rec.Reason = "failed", sp("typecheck_no_package")
		return
	}
	rec.AnalysisEngine = "snapshot_typecheck"
	extractFacts(l, rec, &view{pkg: pkg, info: info, file: file, tf: tf, text: snapshot}, caret, targetText, policy, lim)
}

// view is a type-checked state in which facts at the caret are read: either the per-caret snapshot check
// (snapshot_typecheck) or the cached check of the original package (original_scope).
type view struct {
	pkg  *types.Package
	info *types.Info
	file *ast.File
	tf   *token.File
	text []byte // bytes the caret offset indexes (snapshot or original); only text before the caret is read
	// original_scope only:
	original     bool
	identPresent func(string) bool // is a name present in the snapshot package sources
	strictCut    bool              // strict_prefix: drop declarations of this file at/after the caret
}

func extractFacts(l *Loader, rec *Record, v *view, caret int, targetText, policy string, lim limits) {
	pkg, info, file, tf := v.pkg, v.info, v.file, v.tf
	snapshot := v.text
	pos := tf.Pos(caret)
	// minimal display: package name as written at use sites (not the full import path); own package unqualified
	qual := func(p *types.Package) string {
		if p == pkg {
			return ""
		}
		return p.Name()
	}
	tstr := func(t types.Type) *string {
		if t == nil || !validType(t) {
			return nil
		}
		return sp(types.TypeString(t, qual))
	}

	// imports of this file
	unres := map[string]bool{}
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, "`\"")
		var name string
		var resolved bool
		if obj, ok := info.Defs[imp.Name]; ok && imp.Name != nil && obj != nil {
			name = obj.Name()
		}
		if pn, ok := info.Implicits[imp].(*types.PkgName); ok && info.Implicits != nil {
			name = pn.Name()
		}
		if p := lookupImported(pkg, path); p != nil {
			resolved = true
			if name == "" {
				name = p.Name()
			}
		} else {
			unres[path] = true
		}
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "" {
			name = lastElem(path)
		}
		rec.Imports = append(rec.Imports, ImportFact{Name: name, Path: path, Resolved: resolved})
	}
	for p := range unres {
		rec.UnresolvedImports = append(rec.UnresolvedImports, p)
	}
	sort.Strings(rec.UnresolvedImports)

	// enclosing function
	var encl ast.Node
	var enclDecl *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil || !(n.Pos() <= pos && pos <= n.End()) {
			return n == nil || n == file
		}
		switch x := n.(type) {
		case *ast.FuncDecl:
			if x.Body != nil && x.Body.Lbrace < pos {
				encl, enclDecl = x, x
			}
		case *ast.FuncLit:
			if x.Body != nil && x.Body.Lbrace < pos {
				encl = x
			}
		}
		return true
	})
	var sig, declSig *types.Signature // innermost function (closure or decl) / enclosing top-level FuncDecl
	if enclDecl != nil {
		if obj, ok := info.Defs[enclDecl.Name].(*types.Func); ok {
			sig, _ = obj.Type().(*types.Signature)
			declSig = sig
			name := enclDecl.Name.Name
			if sig != nil && sig.Recv() != nil {
				rt := types.TypeString(sig.Recv().Type(), qual)
				rec.EnclosingType = sp(strings.TrimPrefix(rt, "*"))
				name = "(" + rt + ")." + name
				rec.EnclosingKind = sp("method")
			} else {
				rec.EnclosingKind = sp("func")
			}
			rec.EnclosingSymbol = sp(name)
		}
	}
	if fl, ok := encl.(*ast.FuncLit); ok {
		if tv, ok := info.Types[fl]; ok {
			sig, _ = tv.Type.(*types.Signature)
		}
		rec.EnclosingKind = sp("func_literal")
	}
	if encl == nil {
		rec.EnclosingKind = sp("package_level")
	}
	if sig != nil && sig.Results().Len() > 0 {
		rec.ReturnType = tstr(sig.Results())
		if sig.Results().Len() == 1 {
			rec.ReturnType = tstr(sig.Results().At(0).Type())
		}
	}

	// scope facts: walk from the innermost scope to (excluding) the package scope
	paramObjs := map[types.Object]string{}
	if declSig != nil {
		for i := 0; i < declSig.Params().Len(); i++ {
			paramObjs[declSig.Params().At(i)] = "parameter"
		}
		for i := 0; i < declSig.Results().Len(); i++ {
			paramObjs[declSig.Results().At(i)] = "named_result"
		}
		if declSig.Recv() != nil {
			paramObjs[declSig.Recv()] = "receiver"
		}
	}
	// enclosing func literal params also count as parameters
	ast.Inspect(file, func(n ast.Node) bool {
		if fl, ok := n.(*ast.FuncLit); ok && fl.Body != nil && fl.Body.Lbrace < pos && pos <= fl.Body.End() {
			if t, ok := info.Types[fl].Type.(*types.Signature); ok {
				for i := 0; i < t.Params().Len(); i++ {
					paramObjs[t.Params().At(i)] = "closure_parameter"
				}
			}
		}
		return true
	})
	scope := pkg.Scope().Innermost(pos)
	// Visibility is evaluated just before the caret: on a snapshot the statement being typed may end exactly at
	// the caret (`x := foo.▮` + newline), which would otherwise make its own declarator x visible.
	visPos := pos - 1
	seen := map[string]bool{}
	for s := scope; s != nil && s != pkg.Scope() && s != types.Universe; s = s.Parent() {
		if isFileScope(pkg, s) {
			break
		}
		for _, name := range s.Names() {
			if name == "_" || seen[name] {
				continue
			}
			obj := s.Lookup(name)
			if _, o := scope.LookupParent(name, visPos); o != obj {
				continue // shadowed or not yet declared at the caret
			}
			if obj.Pos() >= pos {
				continue
			}
			seen[name] = true
			f := Fact{Name: name, Type: tstr(obj.Type()), obj: obj}
			switch o := obj.(type) {
			case *types.Var:
				if k, ok := paramObjs[o]; ok {
					f.Kind = k
					rec.Parameters = append(rec.Parameters, f)
					continue
				}
				f.Kind = "local"
			case *types.Const:
				f.Kind = "local_const"
			case *types.TypeName:
				f.Kind = "local_type"
			case *types.Label:
				continue
			default:
				f.Kind = "local"
			}
			rec.Locals = append(rec.Locals, f)
		}
	}
	sortFacts(rec.Locals)
	sortFacts(rec.Parameters)

	// receiver members (Go's "this")
	if declSig != nil && declSig.Recv() != nil {
		rec.ReceiverMembers = membersOf(declSig.Recv().Type(), pkg, qual, lim.maxMembers, &rec.Truncated)
	}
	// package-level declarations (all files of the package; order-independent in Go)
	var pm []Fact
	for _, name := range pkg.Scope().Names() {
		obj := pkg.Scope().Lookup(name)
		if name == "_" || name == "init" || name == "main" {
			continue
		}
		f := Fact{Name: name, Type: tstr(obj.Type()), obj: obj}
		switch o := obj.(type) {
		case *types.Func:
			f.Kind = "func"
			f.Type = nil
			f.Signature = sp(funcSig(name, o.Type().(*types.Signature), qual))
		case *types.TypeName:
			f.Kind = "type"
			f.Type = sp(kindOfType(o.Type()))
		case *types.Const:
			f.Kind = "const"
		case *types.Var:
			f.Kind = "var"
		default:
			continue
		}
		pm = append(pm, f)
	}
	if len(pm) > lim.maxScope {
		pm, rec.Truncated = pm[:lim.maxScope], true
	}
	rec.PackageMembers = pm

	// member access: selector whose dot is right before the caret or whose Sel contains the caret
	var sel *ast.SelectorExpr
	ast.Inspect(file, func(n ast.Node) bool {
		if s, ok := n.(*ast.SelectorExpr); ok {
			dot := s.X.End()
			if dot+1 == pos || (s.Sel.Pos() <= pos && pos <= s.Sel.End() && s.Sel.Pos() == dot+1) {
				sel = s
			}
		}
		return true
	})
	needReceiver := sel != nil
	if sel != nil {
		if id, ok := sel.X.(*ast.Ident); ok {
			if pn, ok := info.Uses[id].(*types.PkgName); ok {
				rec.ReceiverKind = sp("package")
				rec.ReceiverType = sp(pn.Imported().Path())
				if pn.Imported() != nil && pn.Imported().Scope().Len() > 0 && pn.Imported().Complete() {
					rec.Members = packageMembers(pn.Imported(), qual, lim.maxMembers, &rec.Truncated)
				}
			}
		}
		if rec.ReceiverKind == nil {
			if tv, ok := info.Types[sel.X]; ok && validType(tv.Type) {
				if tv.IsType() {
					rec.ReceiverKind = sp("type")
				} else {
					rec.ReceiverKind = sp("value")
				}
				rec.ReceiverType = tstr(tv.Type)
				rec.Members = membersOf(tv.Type, pkg, qual, lim.maxMembers, &rec.Truncated)
			}
		}
	}

	// expected type / call signature from the prefix tokens of the snapshot (never from the target)
	expectedFromPrefix(l, rec, file, info, snapshot, caret, tf, sig, qual, v.original)

	// TYPE contracts of nearby repository types: from in-scope locals/params/receiver only
	rec.ContextTypes = contextTypes(rec, pkg, scope, pos, declSig, qual, lim)

	// status
	reason := ""
	switch {
	case needReceiver && rec.ReceiverType == nil:
		reason = "unresolved_receiver_type"
	case needReceiver && rec.ReceiverKind != nil && *rec.ReceiverKind == "package" && len(rec.Members) == 0:
		reason = "unresolved_import"
	case len(rec.UnresolvedImports) > 0:
		reason = "external_imports_unresolved"
	}
	if reason != "" {
		rec.Status, rec.Reason = "partially_resolved", sp(reason)
	} else {
		rec.Status, rec.Reason = "resolved", nil
	}

	if v.strictCut {
		cutAfterCaret(rec, l, tf, caret)
	}
	NormalizeSlices(rec)
	audit(rec, l, pkg, file, tf, caret, targetText, policy, info, v.identPresent)
	rec.Prompt = render(rec)
}

// buildSnapshotFile parses the target-free snapshot for type checking. Two parse-robustness devices are applied; both
// use only text visible in the snapshot (never the hidden target), and both are recorded in snapshot_repairs:
//
//   - phantom_selector: a caret right after '.' gets a placeholder identifier "_" (what gopls does for incomplete
//     selectors), so `x.` parses as a selector and x keeps its type.
//   - decl_splice (editor_snapshot only): the Go parser's error recovery inside the edited top-level declaration can
//     swallow the following declarations. Every top-level declaration other than the edited one is byte-identical in
//     the snapshot, so it is taken from the parsed original file; only the edited declaration (plus the package
//     clause and imports) is parsed from the snapshot text. The spliced declarations are visible editor text.
func buildSnapshotFile(l *Loader, filename string, snap []byte, caret, removed int, orig *ast.File, policy string) (*ast.File, *token.File, []string) {
	repairs := []string{}
	if caret > 0 && caret <= len(snap) && snap[caret-1] == '.' && (caret == len(snap) || !isIdentByte(snap[caret])) {
		snap = append(append(append([]byte{}, snap[:caret]...), '_'), snap[caret:]...)
		removed--
		repairs = append(repairs, "phantom_selector")
	}
	var encl ast.Decl
	var tfO *token.File
	hdrEnd := -1
	if orig != nil && policy == "editor_snapshot" {
		tfO = l.Fset.File(orig.FileStart)
		if orig.Name != nil {
			hdrEnd = tfO.Offset(orig.Name.End())
		}
		for _, d := range orig.Decls {
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
				hdrEnd = tfO.Offset(g.End())
				continue
			}
			if tfO.Offset(d.Pos()) <= caret && caret <= tfO.Offset(d.End()) {
				encl = d
			}
		}
	}
	if encl == nil || hdrEnd < 0 || tfO.Offset(encl.Pos()) < hdrEnd {
		f, _ := parser.ParseFile(l.Fset, filename, snap, parser.SkipObjectResolution|parser.AllErrors)
		if f == nil {
			return nil, nil, repairs
		}
		tf := l.Fset.File(f.FileStart)
		if tf != nil && caret <= tf.Size() {
			StripBodies(f, tf.Pos(caret)) // only the edited function is re-bound
		}
		return f, tf, repairs
	}
	ds := tfO.Offset(encl.Pos())
	de := tfO.Offset(encl.End()) - removed // end of the edited declaration in snapshot coordinates
	blank := append([]byte{}, snap...)
	for k := range blank {
		if (k >= hdrEnd && k < ds) || k >= de {
			if blank[k] != '\n' {
				blank[k] = ' '
			}
		}
	}
	f, _ := parser.ParseFile(l.Fset, filename, blank, parser.SkipObjectResolution|parser.AllErrors)
	if f == nil {
		return nil, nil, repairs
	}
	if tf := l.Fset.File(f.FileStart); tf != nil && caret <= tf.Size() {
		StripBodies(f, tf.Pos(caret))
	}
	for _, d := range orig.Decls {
		if d == encl {
			continue
		}
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
			continue
		}
		f.Decls = append(f.Decls, skeletonDecl(d))
	}
	repairs = append(repairs, "decl_splice")
	return f, l.Fset.File(f.FileStart), repairs
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

func NormalizeSlices(r *Record) {
	if r.Leakage.TargetIdentifiers == nil {
		r.Leakage.TargetIdentifiers = []string{}
	}
	if r.Leakage.Mentioned == nil {
		r.Leakage.Mentioned = []string{}
	}
	if r.Leakage.Violations == nil {
		r.Leakage.Violations = []string{}
	}
	if r.Locals == nil {
		r.Locals = []Fact{}
	}
	if r.Parameters == nil {
		r.Parameters = []Fact{}
	}
	if r.ReceiverMembers == nil {
		r.ReceiverMembers = []Fact{}
	}
	if r.PackageMembers == nil {
		r.PackageMembers = []Fact{}
	}
	if r.Members == nil {
		r.Members = []Fact{}
	}
	if r.Imports == nil {
		r.Imports = []ImportFact{}
	}
	if r.ContextTypes == nil {
		r.ContextTypes = []TypeContract{}
	}
	if r.UnresolvedImports == nil {
		r.UnresolvedImports = []string{}
	}
	if r.SnapshotRepairs == nil {
		r.SnapshotRepairs = []string{}
	}
}

// isGenericCallee reports whether the call's recorded signature may depend on its arguments: functions with their
// own type parameters, explicit instantiations, and builtins.
func isGenericCallee(e ast.Expr, info *types.Info) bool {
	var id *ast.Ident
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		id = x
	case *ast.SelectorExpr:
		id = x.Sel
	case *ast.IndexExpr, *ast.IndexListExpr:
		return true
	}
	if id == nil {
		return false
	}
	if _, ok := info.Uses[id].(*types.Builtin); ok {
		return true // len/append/make/delete/...: go/types records a signature instantiated from the actual arguments
	}
	if f, ok := info.Uses[id].(*types.Func); ok {
		if s, ok := f.Type().(*types.Signature); ok && s.TypeParams().Len() > 0 {
			return true
		}
	}
	return false
}

func paramName(s *types.Signature, idx int) *string {
	n := s.Params().Len()
	if n == 0 {
		return nil
	}
	if idx >= n {
		if !s.Variadic() {
			return nil
		}
		idx = n - 1
	}
	if name := s.Params().At(idx).Name(); name != "" && name != "_" {
		return sp(name)
	}
	return nil
}

// calleeFunc returns the declared function object of a call's callee (nil for builtins / func values).
func calleeFunc(e ast.Expr, info *types.Info) *types.Func {
	var id *ast.Ident
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		id = x
	case *ast.SelectorExpr:
		id = x.Sel
	case *ast.IndexExpr:
		return calleeFunc(x.X, info)
	case *ast.IndexListExpr:
		return calleeFunc(x.X, info)
	}
	if id == nil {
		return nil
	}
	f, _ := info.Uses[id].(*types.Func)
	return f
}

// cutAfterCaret (strict_prefix on the original engine) drops facts declared in this file at or after the caret.
func cutAfterCaret(rec *Record, l *Loader, tf *token.File, caret int) {
	after := func(o types.Object) bool {
		return o != nil && o.Pos().IsValid() && l.Fset.File(o.Pos()) == tf && tf.Offset(o.Pos()) >= caret
	}
	keep := func(fs []Fact) []Fact {
		out := fs[:0:0]
		for _, f := range fs {
			if !after(f.obj) {
				out = append(out, f)
			}
		}
		return out
	}
	rec.PackageMembers = keep(rec.PackageMembers)
	rec.ReceiverMembers = keep(rec.ReceiverMembers)
	rec.Members = keep(rec.Members)
	var cts []TypeContract
	for _, ct := range rec.ContextTypes {
		if after(ct.obj) {
			continue
		}
		ct.Members = keep(ct.Members)
		cts = append(cts, ct)
	}
	rec.ContextTypes = cts
}

func isFileScope(pkg *types.Package, s *types.Scope) bool { return s.Parent() == pkg.Scope() }

func lookupImported(pkg *types.Package, path string) *types.Package {
	for _, p := range pkg.Imports() {
		if p.Path() == path && p.Complete() {
			return p
		}
	}
	return nil
}

func lastElem(p string) string {
	parts := strings.Split(p, "/")
	return parts[len(parts)-1]
}

func validType(t types.Type) bool {
	if t == nil {
		return false
	}
	if b, ok := t.(*types.Basic); ok && b.Kind() == types.Invalid {
		return false
	}
	return !strings.Contains(types.TypeString(t, nil), "invalid type")
}

func sortFacts(f []Fact) { sort.SliceStable(f, func(i, j int) bool { return f[i].Name < f[j].Name }) }

func kindOfType(t types.Type) string {
	switch t.Underlying().(type) {
	case *types.Struct:
		return "struct"
	case *types.Interface:
		return "interface"
	case *types.Signature:
		return "func"
	case *types.Map:
		return "map"
	case *types.Slice:
		return "slice"
	default:
		return "named"
	}
}

func funcSig(name string, s *types.Signature, q types.Qualifier) string {
	return name + strings.TrimPrefix(types.TypeString(s, q), "func")
}

func packageMembers(p *types.Package, q types.Qualifier, max int, trunc *bool) []Fact {
	var out []Fact
	for _, name := range p.Scope().Names() {
		obj := p.Scope().Lookup(name)
		if !obj.Exported() {
			continue
		}
		f := Fact{Name: name, obj: obj}
		switch o := obj.(type) {
		case *types.Func:
			f.Kind = "func"
			f.Signature = sp(funcSig(name, o.Type().(*types.Signature), q))
		case *types.TypeName:
			f.Kind = "type"
			f.Type = sp(kindOfType(o.Type()))
		case *types.Const:
			f.Kind = "const"
			f.Type = sp(types.TypeString(o.Type(), q))
		case *types.Var:
			f.Kind = "var"
			f.Type = sp(types.TypeString(o.Type(), q))
		default:
			continue
		}
		out = append(out, f)
	}
	if len(out) > max {
		out, *trunc = out[:max], true
	}
	return out
}

// membersOf lists fields (incl. promoted) and methods of t accessible from pkg; methods of *T are included for
// addressable values. Unexported members of other packages are not completion-eligible and are skipped.
func membersOf(t types.Type, pkg *types.Package, q types.Qualifier, max int, trunc *bool) []Fact {
	seen := map[string]bool{}
	var out []Fact
	accessible := func(obj types.Object) bool { return obj.Exported() || obj.Pkg() == pkg }
	ms := types.NewMethodSet(t)
	if _, isPtr := t.(*types.Pointer); !isPtr && !types.IsInterface(t) {
		ms = types.NewMethodSet(types.NewPointer(t))
	}
	var fields []Fact
	var collect func(st *types.Struct, depth int)
	collect = func(st *types.Struct, depth int) {
		for i := 0; i < st.NumFields(); i++ {
			f := st.Field(i)
			if accessible(f) && !seen[f.Name()] {
				seen[f.Name()] = true
				fields = append(fields, Fact{Name: f.Name(), Kind: "field", Type: sp(types.TypeString(f.Type(), q)), obj: f})
			}
			if f.Embedded() && depth < 3 {
				et := f.Type()
				if p, ok := et.(*types.Pointer); ok {
					et = p.Elem()
				}
				if s, ok := et.Underlying().(*types.Struct); ok {
					collect(s, depth+1)
				}
			}
		}
	}
	base := t
	if p, ok := base.(*types.Pointer); ok {
		base = p.Elem()
	}
	if st, ok := base.Underlying().(*types.Struct); ok {
		collect(st, 0)
	}
	var methods []Fact
	for i := 0; i < ms.Len(); i++ {
		m := ms.At(i).Obj()
		if !accessible(m) || seen[m.Name()] {
			continue
		}
		seen[m.Name()] = true
		methods = append(methods, Fact{Name: m.Name(), Kind: "method", Signature: sp(funcSig(m.Name(), m.Type().(*types.Signature), q)), obj: m})
	}
	out = append(fields, methods...)
	if len(out) > max {
		out, *trunc = out[:max], true
	}
	return out
}

// expectedFromPrefix infers the expected type from tokens before the caret on the caret's statement:
// return position (enclosing results), call argument (callee signature parameter), assignment (left-hand side).
func expectedFromPrefix(l *Loader, rec *Record, file *ast.File, info *types.Info, snap []byte, caret int, tf *token.File,
	sig *types.Signature, q types.Qualifier, declaredOnly bool) {
	ls := caret
	for ls > 0 && snap[ls-1] != '\n' {
		ls--
	}
	type tk struct {
		t   token.Token
		off int
	}
	var toks []tk
	fs := token.NewFileSet()
	f := fs.AddFile("", -1, caret-ls)
	var s scanner.Scanner
	s.Init(f, snap[ls:caret], func(token.Position, string) {}, 0)
	for {
		p, t, lit := s.Scan()
		if t == token.EOF {
			break
		}
		if t == token.SEMICOLON && lit != ";" {
			continue
		}
		toks = append(toks, tk{t, ls + f.Offset(p)})
	}
	if len(toks) == 0 {
		return
	}
	// innermost unmatched '(' on this line (call argument position)
	depth, commas := 0, 0
	for i := len(toks) - 1; i >= 0; i-- {
		switch toks[i].t {
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth++
		case token.LBRACK, token.LBRACE:
			if depth == 0 {
				i = -1
				break
			}
			depth--
		case token.COMMA:
			if depth == 0 {
				commas++
			}
		case token.LPAREN:
			if depth > 0 {
				depth--
				continue
			}
			lp := tf.Pos(toks[i].off)
			var callee ast.Expr
			ast.Inspect(file, func(n ast.Node) bool {
				if e, ok := n.(ast.Expr); ok && e.End() == lp {
					if tv, ok := info.Types[e]; ok {
						if _, ok := tv.Type.Underlying().(*types.Signature); ok && !tv.IsType() {
							callee = e
						}
					}
				}
				return true
			})
			if callee == nil {
				return
			}
			cs := info.Types[callee].Type.Underlying().(*types.Signature)
			if declaredOnly && isGenericCallee(callee, info) {
				// On the full original file the recorded signature of a generic function or builtin is instantiated
				// from ALL arguments, including those in the hidden target. Only the declared (uninstantiated)
				// signature of a generic function is emitted, and no expected type; builtins get nothing.
				f := calleeFunc(callee, info)
				if f == nil {
					return
				}
				idx := commas
				rec.CallName = sp(types.ExprString(callee))
				rec.CallSignature = sp(strings.TrimPrefix(types.TypeString(f.Type(), q), "func"))
				rec.ArgumentIndex = &idx
				rec.ParameterName = paramName(f.Type().(*types.Signature), idx)
				return
			}
			rec.CallName = sp(types.ExprString(callee)) // source text before the '(' (prefix only)
			rec.CallSignature = sp(strings.TrimPrefix(types.TypeString(cs, q), "func"))
			idx := commas
			rec.ArgumentIndex = &idx
			rec.ParameterName = paramName(cs, idx)
			n := cs.Params().Len()
			var pt types.Type
			switch {
			case cs.Variadic() && idx >= n-1 && n > 0:
				if sl, ok := cs.Params().At(n - 1).Type().(*types.Slice); ok {
					pt = sl.Elem()
				}
			case idx < n:
				pt = cs.Params().At(idx).Type()
			}
			if pt != nil && validType(pt) {
				rec.ExpectedType = sp(types.TypeString(pt, q))
				rec.ExpectedTypeSource = sp("argument")
			}
			return
		}
	}
	// return statement
	if toks[0].t == token.RETURN && sig != nil {
		idx := 0
		d := 0
		for _, t := range toks[1:] {
			switch t.t {
			case token.LPAREN, token.LBRACK, token.LBRACE:
				d++
			case token.RPAREN, token.RBRACK, token.RBRACE:
				d--
			case token.COMMA:
				if d == 0 {
					idx++
				}
			}
		}
		if idx < sig.Results().Len() {
			if t := sig.Results().At(idx).Type(); validType(t) {
				rec.ExpectedType = sp(types.TypeString(t, q))
				rec.ExpectedTypeSource = sp("return")
			}
		}
		return
	}
	// assignment "lhs = " (single lhs; := has no expected type)
	last := toks[len(toks)-1]
	if last.t == token.ASSIGN && len(toks) >= 2 {
		eq := tf.Pos(last.off)
		var lhsT types.Type
		ast.Inspect(file, func(n ast.Node) bool {
			if as, ok := n.(*ast.AssignStmt); ok && as.TokPos == eq && len(as.Lhs) == 1 {
				if tv, ok := info.Types[as.Lhs[0]]; ok {
					lhsT = tv.Type
				}
			}
			// `var x T = ▮`: the '=' directly follows the declared type of this spec
			if vs, ok := n.(*ast.ValueSpec); ok && vs.Type != nil && len(vs.Names) == 1 && vs.Type.End() <= eq &&
				l.Fset.File(vs.Type.End()) == tf {
				if a, b := tf.Offset(vs.Type.End()), tf.Offset(eq); a <= b && strings.TrimSpace(string(snap[a:b])) == "" {
					if tv, ok := info.Types[vs.Type]; ok {
						lhsT = tv.Type
					}
				}
			}
			return true
		})
		if lhsT != nil && validType(lhsT) {
			rec.ExpectedType = sp(types.TypeString(lhsT, q))
			rec.ExpectedTypeSource = sp("initializer")
			if !strings.HasPrefix(strings.TrimSpace(string(snap[ls:caret])), "var ") {
				rec.ExpectedTypeSource = sp("assignment")
			}
		}
	}
	return
}

// contextTypes: contracts of repository-defined named types reachable from in-scope facts (never from the target).
func contextTypes(rec *Record, pkg *types.Package, scope *types.Scope, pos token.Pos, sig *types.Signature,
	q types.Qualifier, lim limits) []TypeContract {
	type cand struct {
		t      *types.Named
		source string
	}
	var cands []cand
	seen := map[*types.TypeName]bool{}
	var addT func(t types.Type, src string, depth int)
	addT = func(t types.Type, src string, depth int) {
		if t == nil || depth > 2 {
			return
		}
		switch x := t.(type) {
		case *types.Pointer:
			addT(x.Elem(), src, depth+1)
		case *types.Slice:
			addT(x.Elem(), src, depth+1)
		case *types.Map:
			addT(x.Key(), src, depth+1)
			addT(x.Elem(), src, depth+1)
		case *types.Named:
			o := x.Obj()
			if o.Pkg() == nil || seen[o] {
				return
			}
			// repository types only: same package or another package of the analyzed module
			if !(o.Pkg() == pkg || sameModule(o.Pkg().Path(), pkg.Path())) {
				return
			}
			seen[o] = true
			cands = append(cands, cand{x, src})
			for i := 0; i < x.TypeArgs().Len(); i++ {
				addT(x.TypeArgs().At(i), src, depth+1)
			}
		}
	}
	var enclosingRecv *types.TypeName
	if sig != nil && sig.Recv() != nil {
		rt := sig.Recv().Type()
		if p, ok := rt.(*types.Pointer); ok {
			rt = p.Elem()
		}
		if n, ok := rt.(*types.Named); ok {
			enclosingRecv = n.Obj()
			seen[enclosingRecv] = true // already described by receiver_members
		}
	}
	for s := scope; s != nil && s != pkg.Scope(); s = s.Parent() {
		if isFileScope(pkg, s) {
			break
		}
		for _, name := range s.Names() {
			obj := s.Lookup(name)
			if v, ok := obj.(*types.Var); ok && obj.Pos() < pos {
				if _, o := scope.LookupParent(name, pos-1); o == obj {
					addT(v.Type(), "local_or_parameter", 0)
				}
			}
		}
	}
	if enclosingRecv != nil {
		if st, ok := enclosingRecv.Type().Underlying().(*types.Struct); ok {
			for i := 0; i < st.NumFields(); i++ {
				addT(st.Field(i).Type(), "receiver_field", 0)
			}
		}
	}
	var out []TypeContract
	for _, c := range cands {
		if rec.ReceiverType != nil && types.TypeString(c.t, q) == strings.TrimPrefix(*rec.ReceiverType, "*") {
			continue // already in MEMBER
		}
		var tr bool
		mem := membersOf(c.t, pkg, q, 1<<30, &tr)
		total := len(mem)
		if len(mem) > lim.maxTypeMembers {
			mem = mem[:lim.maxTypeMembers]
		}
		if mem == nil {
			mem = []Fact{}
		}
		out = append(out, TypeContract{Name: types.TypeString(c.t, q), Kind: kindOfType(c.t), Source: c.source,
			Members: mem, TotalMembers: total, obj: c.t.Obj()})
		if len(out) >= lim.maxTypes {
			break
		}
	}
	return out
}

func sameModule(a, b string) bool {
	pa, pb := strings.Split(a, "/"), strings.Split(b, "/")
	if len(pa) < 3 || len(pb) < 3 {
		return a == b
	}
	return pa[0] == pb[0] && pa[1] == pb[1] && pa[2] == pb[2]
}

// audit computes the leakage report. Violations (must be empty; `validate` fails otherwise):
//   - local_declared_after_caret:<name>    a local/parameter fact declared at/after the caret in the snapshot file
//   - declared_in_synthetic_suffix:<name>  (strict_prefix) same, i.e. declared by the appended closers
//   - name_only_in_target:<name>           a fact declared in the analyzed package whose name occurs in the hidden target
//     and nowhere in the snapshot package sources: it can only have come from the target text
//
// Facts declared in other packages (std, other repository packages, vendor) come from separately type-checked
// packages that never saw the snapshot, so overlap with the target is legitimate coverage ("mentioned").
func audit(rec *Record, l *Loader, pkg *types.Package, file *ast.File, tf *token.File, caret int, target, policy string,
	info *types.Info, identPresent func(string) bool) {
	tids := map[string]bool{}
	var s scanner.Scanner
	fs := token.NewFileSet()
	f := fs.AddFile("", -1, len(target))
	s.Init(f, []byte(target), func(token.Position, string) {}, 0)
	for {
		_, t, lit := s.Scan()
		if t == token.EOF {
			break
		}
		if t == token.IDENT {
			tids[lit] = true
		}
	}
	present := identPresent
	if present == nil {
		snapIdents := map[string]bool{}
		for id := range info.Defs {
			snapIdents[id.Name] = true
		}
		for id := range info.Uses {
			snapIdents[id.Name] = true
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				snapIdents[id.Name] = true
			}
			return true
		})
		present = func(n string) bool { return snapIdents[n] }
	}
	var mentioned, violations []string
	check := func(facts []Fact, local bool) {
		for _, fct := range facts {
			if tids[fct.Name] {
				mentioned = append(mentioned, fct.Name)
				if fct.obj != nil && fct.obj.Pkg() == pkg && !present(fct.Name) {
					violations = append(violations, "name_only_in_target:"+fct.Name)
				}
			}
			if local && fct.obj != nil && l.Fset.File(fct.obj.Pos()) == tf && tf.Offset(fct.obj.Pos()) >= caret {
				if policy == "strict_prefix" {
					violations = append(violations, "declared_in_synthetic_suffix:"+fct.Name)
				} else {
					violations = append(violations, "local_declared_after_caret:"+fct.Name)
				}
			}
		}
	}
	check(rec.Locals, true)
	check(rec.Parameters, true)
	check(rec.ReceiverMembers, false)
	check(rec.PackageMembers, false)
	check(rec.Members, false)
	for _, ct := range rec.ContextTypes {
		check(ct.Members, false)
	}
	sort.Strings(mentioned)
	mentioned = dedup(mentioned)
	sort.Strings(violations)
	violations = dedup(violations)
	ids := make([]string, 0, len(tids))
	for k := range tids {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	if mentioned == nil {
		mentioned = []string{}
	}
	if violations == nil {
		violations = []string{}
	}
	rec.Leakage = Leakage{TargetIdentifiers: ids, Mentioned: mentioned, Violations: violations}
}

func dedup(s []string) []string {
	var out []string
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// render is a compact debug rendering (the model-facing format is produced by a separate, versioned render step).
func render(r *Record) string {
	var b strings.Builder
	line := func(k string, items []string) {
		if len(items) > 0 {
			fmt.Fprintf(&b, "%s %s\n", k, strings.Join(items, " "))
		}
	}
	tn := func(fs []Fact) []string {
		var out []string
		for _, f := range fs {
			switch {
			case f.Signature != nil:
				out = append(out, *f.Signature)
			case f.Type != nil:
				out = append(out, f.Name+":"+*f.Type)
			default:
				out = append(out, f.Name)
			}
		}
		return out
	}
	if r.ExpectedType != nil {
		line("EXPECT", []string{*r.ExpectedType})
	}
	if r.ReceiverType != nil {
		line("RECV", []string{*r.ReceiverType})
	}
	if r.CallSignature != nil {
		line("CALL", []string{*r.CallSignature})
	}
	line("ARG", tn(r.Parameters))
	line("LOCAL", tn(r.Locals))
	if r.ReturnType != nil {
		line("RET", []string{*r.ReturnType})
	}
	line("MEMBER", tn(r.Members))
	line("THIS", tn(r.ReceiverMembers))
	for _, ct := range r.ContextTypes {
		line("TYPE "+ct.Name+":", tn(ct.Members))
	}
	return strings.TrimRight(b.String(), "\n")
}
