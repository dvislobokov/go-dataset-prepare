package flc

import (
	"crypto/sha256"
	"encoding/binary"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------------------------------------- hashing

// Uniform maps '\x1f'-joined parts to [0,1) exactly like the C# Hashing.Uniform (top 53 bits of SHA-256, big endian).
func Uniform(parts ...string) float64 {
	d := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return float64(binary.BigEndian.Uint64(d[:8])>>11) / float64(uint64(1)<<53)
}

// StableID = first 32 hex chars of SHA-256 over '\x1f'-joined parts (same as C# Hashing.StableId).
func StableID(parts ...string) string {
	return Sha256Hex([]byte(strings.Join(parts, "\x1f")))[:32]
}

// ---------------------------------------------------------------------------------------------------------- tokens

type tok struct {
	t          token.Token
	start, end int // byte offsets into the original bytes
	lit        string
	ctx        string // bracket context at this token: call|params|paren|index|composite|block|""
}

func (k tok) isComment() bool { return k.t == token.COMMENT }

// scanTokens returns all real tokens (auto-inserted semicolons dropped) with exact source spans. go/scanner strips CR
// from comment and raw-string literals, so spans are recomputed from the source, never from len(lit).
func scanTokens(src []byte) (toks []tok, nerr int) {
	fset := token.NewFileSet()
	f := fset.AddFile("", -1, len(src))
	var s scanner.Scanner
	s.Init(f, src, func(token.Position, string) { nerr++ }, scanner.ScanComments)
	for {
		pos, t, lit := s.Scan()
		if t == token.EOF {
			break
		}
		if t == token.SEMICOLON && lit != ";" {
			continue // auto-inserted
		}
		start := f.Offset(pos)
		end := start
		switch t {
		case token.COMMENT:
			if strings.HasPrefix(lit, "//") {
				end = start
				for end < len(src) && src[end] != '\n' {
					end++
				}
				if end > start && src[end-1] == '\r' {
					end--
				}
			} else {
				i := strings.Index(string(src[start+2:]), "*/")
				if i < 0 {
					end = len(src)
				} else {
					end = start + 2 + i + 2
				}
			}
		case token.STRING:
			if src[start] == '`' {
				i := strings.IndexByte(string(src[start+1:]), '`')
				if i < 0 {
					end = len(src)
				} else {
					end = start + 1 + i + 1
				}
			} else {
				end = start + len(lit)
			}
		case token.IDENT, token.INT, token.FLOAT, token.IMAG, token.CHAR:
			end = start + len(lit)
		case token.ILLEGAL:
			end = start + max(1, len(lit))
		default:
			if t.IsKeyword() || t.IsOperator() {
				end = start + len(t.String())
			} else {
				end = start + max(1, len(lit))
			}
		}
		if end > len(src) {
			end = len(src)
		}
		toks = append(toks, tok{t: t, start: start, end: end, lit: lit})
	}
	return toks, nerr
}

// ---------------------------------------------------------------------------------------------------------- AST facts

type span struct {
	start, end int
	tag        string
}

type astFacts struct {
	stmtKind     map[int]string // offset -> kind of the outermost statement/declaration/element starting there
	callLparen   map[int]bool
	paramsLparen map[int]bool
	compositeLb  map[int]bool
	funcLits     map[int]string // FuncLit start -> parent subkind
	errIf        map[int]string // IfStmt start -> if_err_check|if_err_init|if_err_is
	errIfCond    map[int]bool   // offset of the condition (or init) of an error check
	errBodyStmt  map[int]bool
	errCtor      map[int]string // Lparen of fmt.Errorf/errors.New/... -> subkind
	logTemplate  map[int]string // string literal start -> log/format kind
	importNames  map[string]bool
	spans        []span
}

var formatFuncs = map[string]int{ // name -> index of the format argument
	"Printf": 0, "Sprintf": 0, "Errorf": 0, "Fatalf": 0, "Panicf": 0, "Logf": 0, "Infof": 0, "Debugf": 0, "Warnf": 0,
	"Warningf": 0, "Tracef": 0, "Noticef": 0, "Criticalf": 0, "Skipf": 0, "Msgf": 0, "Fprintf": 1, "Appendf": 1,
}

var messageFuncs = map[string]int{
	"Info": 0, "Debug": 0, "Warn": 0, "Warning": 0, "Error": 0, "Fatal": 0, "Panic": 0, "Print": 0, "Println": 0,
	"Trace": 0, "Msg": 0, "InfoContext": 1, "DebugContext": 1, "WarnContext": 1, "ErrorContext": 1, "Log": 0,
}

var loggerRecv = regexp.MustCompile(`(?i)^(log|slog|logger|l|lg|logrus|zap|klog|glog|log15|zerolog|t|b|tb|s|sugar)$|(?i)log(ger)?$`)

var errCtorNames = map[string]bool{"New": true, "Wrap": true, "Wrapf": true, "Errorf": true, "WithMessage": true,
	"WithMessagef": true, "Is": true, "As": true, "Join": true, "Unwrap": true, "WithStack": true}

func isErrIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	if !ok {
		return false
	}
	n := id.Name
	return n == "err" || strings.HasSuffix(n, "Err") || strings.HasSuffix(n, "err")
}

func isNil(e ast.Expr) bool { id, ok := e.(*ast.Ident); return ok && id.Name == "nil" }

func recvName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.CallExpr:
		return recvName(x.Fun)
	}
	return ""
}

func importName(spec *ast.ImportSpec) string {
	if spec.Name != nil {
		return spec.Name.Name
	}
	p, err := strconv.Unquote(spec.Path.Value)
	if err != nil {
		return ""
	}
	parts := strings.Split(p, "/")
	last := parts[len(parts)-1]
	if len(parts) > 1 && len(last) >= 2 && last[0] == 'v' && strings.Trim(last[1:], "0123456789") == "" {
		last = parts[len(parts)-2] // github.com/x/y/v5 -> y
	}
	if i := strings.Index(last, "."); i >= 0 {
		last = last[:i] // gopkg.in/yaml.v3 -> yaml
	}
	last = strings.TrimPrefix(last, "go-")
	return strings.ReplaceAll(last, "-", "_")
}

func collectAST(file *ast.File, tf *token.File) *astFacts {
	a := &astFacts{stmtKind: map[int]string{}, callLparen: map[int]bool{}, paramsLparen: map[int]bool{},
		compositeLb: map[int]bool{}, funcLits: map[int]string{}, errIf: map[int]string{}, errIfCond: map[int]bool{},
		errBodyStmt: map[int]bool{}, errCtor: map[int]string{}, logTemplate: map[int]string{}, importNames: map[string]bool{}}
	if file == nil {
		return a
	}
	off := func(p token.Pos) int {
		if !p.IsValid() || int(p) < tf.Base() || int(p) > tf.Base()+tf.Size() {
			return -1
		}
		return tf.Offset(p)
	}
	set := func(p token.Pos, kind string) {
		if o := off(p); o >= 0 {
			if _, ok := a.stmtKind[o]; !ok {
				a.stmtKind[o] = kind
			}
		}
	}
	addSpan := func(n ast.Node, tag string) {
		s, e := off(n.Pos()), off(n.End())
		if s >= 0 && e >= s {
			a.spans = append(a.spans, span{s, e, tag})
		}
	}
	for _, imp := range file.Imports {
		if n := importName(imp); n != "" && n != "_" && n != "." {
			a.importNames[n] = true
		}
	}
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		var parent ast.Node
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		stack = append(stack, n)
		switch x := n.(type) {
		case *ast.GenDecl:
			set(x.Pos(), strings.ToLower(x.Tok.String())+"_decl")
		case *ast.FuncDecl:
			if x.Recv != nil {
				set(x.Pos(), "method_decl")
			} else {
				set(x.Pos(), "func_decl")
			}
		case *ast.ImportSpec:
			set(x.Pos(), "import_spec")
		case *ast.ValueSpec:
			set(x.Pos(), "value_spec")
		case *ast.TypeSpec:
			set(x.Pos(), "type_spec")
			if x.TypeParams != nil {
				addSpan(x.TypeParams, "generics")
			}
		case *ast.Field:
			set(x.Pos(), "field")
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				set(x.Pos(), "short_var_decl")
			} else {
				set(x.Pos(), "assignment")
			}
		case *ast.ExprStmt:
			if _, ok := x.X.(*ast.CallExpr); ok {
				set(x.Pos(), "call_stmt")
			} else {
				set(x.Pos(), "expr_stmt")
			}
		case *ast.SendStmt:
			set(x.Pos(), "send")
		case *ast.IncDecStmt:
			set(x.Pos(), "inc_dec")
		case *ast.ReturnStmt:
			set(x.Pos(), "return")
		case *ast.IfStmt:
			set(x.Pos(), "if")
			kind := ""
			if be, ok := x.Cond.(*ast.BinaryExpr); ok && be.Op == token.NEQ && isErrIdent(be.X) && isNil(be.Y) {
				kind = "if_err_check"
				if x.Init != nil {
					kind = "if_err_init"
				}
			} else if ce, ok := x.Cond.(*ast.CallExpr); ok {
				if se, ok := ce.Fun.(*ast.SelectorExpr); ok && recvName(se.X) == "errors" && (se.Sel.Name == "Is" || se.Sel.Name == "As") {
					kind = "if_err_is"
				}
			}
			if kind != "" {
				if o := off(x.Pos()); o >= 0 {
					a.errIf[o] = kind
				}
				first := ast.Node(x.Cond)
				if x.Init != nil {
					first = x.Init
				}
				if o := off(first.Pos()); o >= 0 {
					a.errIfCond[o] = true
				}
				for _, st := range x.Body.List {
					if o := off(st.Pos()); o >= 0 {
						a.errBodyStmt[o] = true
					}
				}
				addSpan(x.Body, "error_handling")
			}
		case *ast.ForStmt:
			set(x.Pos(), "for")
		case *ast.RangeStmt:
			set(x.Pos(), "range_for")
		case *ast.SwitchStmt:
			set(x.Pos(), "switch")
			addSpan(x, "switch")
		case *ast.TypeSwitchStmt:
			set(x.Pos(), "type_switch")
			addSpan(x, "switch")
		case *ast.SelectStmt:
			set(x.Pos(), "select")
			addSpan(x, "select")
		case *ast.CaseClause:
			set(x.Pos(), "case")
		case *ast.CommClause:
			set(x.Pos(), "comm_case")
		case *ast.GoStmt:
			set(x.Pos(), "go")
			addSpan(x, "goroutine")
		case *ast.DeferStmt:
			set(x.Pos(), "defer")
			addSpan(x, "defer")
		case *ast.BranchStmt:
			set(x.Pos(), "branch")
		case *ast.LabeledStmt:
			set(x.Pos(), "label")
		case *ast.DeclStmt:
			set(x.Pos(), "local_decl")
		case *ast.BlockStmt:
			set(x.Pos(), "block")
		case *ast.FuncType:
			if x.Params != nil {
				if o := off(x.Params.Opening); o >= 0 {
					a.paramsLparen[o] = true
				}
			}
			if x.TypeParams != nil {
				addSpan(x.TypeParams, "generics")
			}
		case *ast.StructType:
			addSpan(x, "struct_type")
		case *ast.InterfaceType:
			addSpan(x, "interface_type")
		case *ast.FuncLit:
			sub := "expression"
			switch p := parent.(type) {
			case *ast.CallExpr:
				sub = "call_argument"
				if p.Fun == n {
					sub = "immediately_invoked"
				}
			case *ast.AssignStmt, *ast.ValueSpec:
				sub = "assignment"
			case *ast.ReturnStmt:
				sub = "return"
			case *ast.KeyValueExpr, *ast.CompositeLit:
				sub = "composite_element"
			}
			if len(stack) >= 3 {
				if c, ok := stack[len(stack)-2].(*ast.CallExpr); ok && c.Fun == n {
					switch stack[len(stack)-3].(type) {
					case *ast.GoStmt:
						sub = "go"
					case *ast.DeferStmt:
						sub = "defer"
					}
				}
			}
			if o := off(x.Pos()); o >= 0 {
				a.funcLits[o] = sub
			}
			addSpan(x.Body, "closure")
		case *ast.CompositeLit:
			if o := off(x.Lbrace); o >= 0 {
				a.compositeLb[o] = true
			}
			addSpan(x, "composite_literal")
			for _, el := range x.Elts {
				if _, ok := el.(*ast.KeyValueExpr); ok {
					set(el.Pos(), "key_value")
				} else {
					set(el.Pos(), "composite_element")
				}
			}
		case *ast.CallExpr:
			if o := off(x.Lparen); o >= 0 {
				a.callLparen[o] = true
			}
			for _, arg := range x.Args {
				set(arg.Pos(), "call_argument")
			}
			if se, ok := x.Fun.(*ast.SelectorExpr); ok {
				name, recv := se.Sel.Name, recvName(se.X)
				if (recv == "errors" && errCtorNames[name]) || (recv == "fmt" && name == "Errorf") {
					if o := off(x.Lparen); o >= 0 {
						a.errCtor[o] = recv + "." + name
					}
				}
				idx, kind := -1, ""
				if i, ok := formatFuncs[name]; ok {
					idx, kind = i, "format"
				} else if i, ok := messageFuncs[name]; ok && loggerRecv.MatchString(recv) {
					idx, kind = i, "message"
				} else if recv == "errors" && name == "New" {
					idx, kind = 0, "error_message"
				}
				if recv == "fmt" && name == "Errorf" {
					kind = "error_message"
				}
				if idx >= 0 && idx < len(x.Args) {
					if bl, ok := x.Args[idx].(*ast.BasicLit); ok && bl.Kind == token.STRING {
						if o := off(bl.Pos()); o >= 0 {
							a.logTemplate[o] = kind
						}
					}
				}
			}
		}
		return true
	})
	return a
}

// ---------------------------------------------------------------------------------------------------------- candidates

type candidate struct {
	off     int
	kind    string
	subkind string
	probe   bool // negative-stratum probe (inside a literal/comment): always excluded unless it is a log template
}

var kindPriority = map[string]int{"log_message": 0, "error_handling": 1, "func_literal": 2, "member_access": 3,
	"line_start": 4, "control_flow": 5, "composite_literal": 6, "argument_list": 7, "after_keyword": 8,
	"after_operator": 9, "identifier_partial": 10, "token_boundary": 11}

var afterKeyword = map[token.Token]bool{token.RETURN: true, token.GO: true, token.DEFER: true, token.RANGE: true,
	token.CASE: true, token.VAR: true, token.CONST: true, token.TYPE: true, token.ELSE: true, token.FUNC: true,
	token.IMPORT: true, token.PACKAGE: true, token.GOTO: true, token.CHAN: true, token.MAP: true}

var controlFlow = map[token.Token]bool{token.IF: true, token.FOR: true, token.SWITCH: true, token.SELECT: true}

var afterOperator = map[token.Token]bool{token.ASSIGN: true, token.DEFINE: true, token.ADD_ASSIGN: true,
	token.SUB_ASSIGN: true, token.MUL_ASSIGN: true, token.QUO_ASSIGN: true, token.REM_ASSIGN: true,
	token.AND_ASSIGN: true, token.OR_ASSIGN: true, token.XOR_ASSIGN: true, token.SHL_ASSIGN: true,
	token.SHR_ASSIGN: true, token.AND_NOT_ASSIGN: true, token.EQL: true, token.NEQ: true, token.LSS: true,
	token.GTR: true, token.LEQ: true, token.GEQ: true, token.LAND: true, token.LOR: true, token.ARROW: true}

// FileResult is everything extracted from one file; counters are merged into the run summary.
type FileResult struct {
	Rel        string
	Samples    []*Sample
	Exclusions []*Exclusion
	Counters   map[string]int
	Corpus     *CorpusFile
	DedupKeys  []string // per sample, for dataset-level duplicate line-target dropping
}

type Extractor struct {
	cfg          *Config
	cfgSha       string
	repo         *RepoInfo
	lineSecrets  []*regexp.Regexp
	writeCorpus  bool
	split, group string
}

func NewExtractor(cfg *Config, repo *RepoInfo, writeCorpus bool) *Extractor {
	e := &Extractor{cfg: cfg, cfgSha: cfg.Sha256(), repo: repo, writeCorpus: writeCorpus, split: cfg.Split.RepositorySplit}
	for _, p := range cfg.Secrets.LinePatterns {
		e.lineSecrets = append(e.lineSecrets, regexp.MustCompile(p))
	}
	if e.split == "" {
		e.split = "train"
	}
	e.group = repo.RepositoryID
	if cfg.Split.RepositoryGroup != nil && *cfg.Split.RepositoryGroup != "" {
		e.group = *cfg.Split.RepositoryGroup
	}
	return e
}

// lineSecretValue applies the line-level secret patterns, ignoring obvious non-secrets (UPPER_SNAKE env-var names).
func (e *Extractor) lineHasSecret(line []byte) bool {
	for _, rx := range e.lineSecrets {
		for _, m := range rx.FindAllSubmatch(line, -1) {
			v := m[len(m)-1]
			if strings.Trim(string(v), "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") == "" {
				continue
			}
			return true
		}
	}
	return false
}

func (e *Extractor) Extract(af *AcceptedFile) *FileResult {
	src := af.Src
	b := src.Original
	res := &FileResult{Rel: af.Rel, Counters: map[string]int{}}
	cnt := func(k string, n int) { res.Counters[k] += n }
	exclusionsPerReason := map[string]int{}
	addExclusion := func(line, off int, reason, kind string) {
		cnt("excluded."+reason, 1)
		if exclusionsPerReason[reason] < e.cfg.Sampling.ExclusionExamplesPerReason {
			exclusionsPerReason[reason]++
			res.Exclusions = append(res.Exclusions, &Exclusion{SchemaVersion: "flc-exclusion/v1", RelativePath: af.Rel,
				Line: line, ByteOffset: off, Reason: reason, CaretKind: kind,
				LineText: string(b[src.LineStarts[line]:src.LineContentEnd[line]])})
		}
	}
	addLineExclusion := func(line int, reason string) {
		cnt("lines.excluded."+reason, 1)
		if exclusionsPerReason["line:"+reason] < e.cfg.Sampling.ExclusionExamplesPerReason {
			exclusionsPerReason["line:"+reason]++
			res.Exclusions = append(res.Exclusions, &Exclusion{SchemaVersion: "flc-exclusion/v1", RelativePath: af.Rel,
				Line: line, ByteOffset: src.LineStarts[line], Reason: "line:" + reason,
				LineText: string(b[src.LineStarts[line]:src.LineContentEnd[line]])})
		}
	}

	fset := token.NewFileSet()
	file, perr := parser.ParseFile(fset, af.Rel, b, parser.ParseComments|parser.SkipObjectResolution|parser.AllErrors)
	syntaxErrors := perr != nil
	var facts *astFacts
	if file != nil {
		facts = collectAST(file, fset.File(file.Pos()))
	} else {
		facts = collectAST(nil, nil)
	}
	toks, scanErrs := scanTokens(b)
	if scanErrs > 0 {
		syntaxErrors = true
	}
	if syntaxErrors {
		cnt("files.with_syntax_errors", 1)
	}

	// bracket context per token
	var stackCtx []string
	for i := range toks {
		k := &toks[i]
		top := ""
		if len(stackCtx) > 0 {
			top = stackCtx[len(stackCtx)-1]
		}
		k.ctx = top
		switch k.t {
		case token.LPAREN:
			c := "paren"
			if facts.callLparen[k.start] {
				c = "call"
			} else if facts.paramsLparen[k.start] {
				c = "params"
			}
			stackCtx = append(stackCtx, c)
		case token.LBRACK:
			stackCtx = append(stackCtx, "index")
		case token.LBRACE:
			c := "block"
			if facts.compositeLb[k.start] {
				c = "composite"
			}
			stackCtx = append(stackCtx, c)
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if len(stackCtx) > 0 {
				stackCtx = stackCtx[:len(stackCtx)-1]
			}
		}
	}

	nLines := src.NumLines()
	// tokens by line (a token belongs to the line where it starts); multi-line tokens recorded separately
	lineToks := make([][]int, nLines)
	type mspan struct {
		start, end int
		t          token.Token
		raw        bool
	}
	var multi []mspan
	coveredBy := make([]int, nLines) // index+1 into multi when the line START is inside a multi-line token
	for i, k := range toks {
		l := src.LineOf(k.start)
		lineToks[l] = append(lineToks[l], i)
		if el := src.LineOf(max(k.start, k.end-1)); el > l {
			multi = append(multi, mspan{k.start, k.end, k.t, k.t == token.STRING})
			for x := l + 1; x <= el && x < nLines; x++ {
				if src.LineStarts[x] < k.end {
					coveredBy[x] = len(multi)
				}
			}
		}
	}
	tokenAt := func(off int) *tok { // token with start < off < end
		i := sort.Search(len(toks), func(i int) bool { return toks[i].start >= off }) - 1
		if i >= 0 && toks[i].start < off && off < toks[i].end {
			return &toks[i]
		}
		return nil
	}

	type eligible struct {
		c         candidate
		line      int
		targetEnd int
		trivial   bool
		u, score  float64
	}
	var elig []eligible
	weights := e.cfg.Sampling.Weights
	maxLine := e.cfg.Sampling.MaxLineChars
	prevLineEndsWithDot := false

	for l := 0; l < nLines; l++ {
		ls, le := src.LineStarts[l], src.LineContentEnd[l]
		cnt("lines.total", 1)
		firstNonWs := ls
		for firstNonWs < le && isHSpace(b[firstNonWs]) {
			firstNonWs++
		}
		if firstNonWs == le && coveredBy[l] == 0 {
			cnt("lines.blank", 1)
			prevLineEndsWithDot = false
			continue
		}
		if m := coveredBy[l]; m > 0 {
			if multi[m-1].raw {
				addLineExclusion(l, "inside_raw_string")
			} else {
				addLineExclusion(l, "inside_block_comment")
			}
			prevLineEndsWithDot = false
			continue
		}
		var code []int
		var lits []int
		for _, i := range lineToks[l] {
			if toks[i].isComment() || toks[i].t == token.STRING || toks[i].t == token.CHAR {
				lits = append(lits, i)
			}
			if !toks[i].isComment() {
				code = append(code, i)
			}
		}
		if len(code) == 0 {
			text := string(b[firstNonWs:le])
			switch {
			case strings.HasPrefix(text, "//go:build") || strings.HasPrefix(text, "// +build"):
				addLineExclusion(l, "build_constraint")
			case strings.HasPrefix(text, "//go:") || strings.HasPrefix(text, "//line ") || strings.HasPrefix(text, "//export ") || strings.HasPrefix(text, "//nolint"):
				addLineExclusion(l, "directive")
			default:
				addLineExclusion(l, "comment")
			}
			prevLineEndsWithDot = false
			continue
		}
		if UTF16Len(string(b[ls:le])) > maxLine {
			addLineExclusion(l, "too_long")
			prevLineEndsWithDot = toks[code[len(code)-1]].t == token.PERIOD
			continue
		}
		cnt("lines.code", 1)
		lineSecret := e.lineHasSecret(b[ls:le])

		cands := map[int]candidate{}
		add := func(off int, kind, sub string) {
			if off < ls || off > le {
				return
			}
			if old, ok := cands[off]; ok && kindPriority[old.kind] <= kindPriority[kind] {
				return
			}
			cands[off] = candidate{off: off, kind: kind, subkind: sub}
		}
		afterSpace := func(off int) int { // the typed separator: all horizontal whitespace (gofmt alignment)
			for off < le && isHSpace(b[off]) {
				off++
			}
			return off
		}
		for ci, ti := range code {
			k := toks[ti]
			var next *tok
			if ti+1 < len(toks) {
				next = &toks[ti+1]
			}
			var prev *tok
			if ti > 0 {
				prev = &toks[ti-1]
			}
			if ci == 0 {
				kind, sub := "line_start", facts.stmtKind[k.start]
				switch {
				case sub != "":
				case k.t == token.RBRACE:
					sub = "closing_brace"
				case k.t == token.RPAREN:
					sub = "closing_paren"
				case k.t == token.RBRACK:
					sub = "closing_bracket"
				case prevLineEndsWithDot:
					sub = "method_chain"
				default:
					sub = "continuation"
				}
				if ek, ok := facts.errIf[k.start]; ok {
					kind, sub = "error_handling", ek
				} else if facts.errBodyStmt[k.start] {
					kind, sub = "error_handling", "err_body_"+sub
				}
				add(k.start, kind, sub)
			}
			if k.t == token.IDENT {
				identPartials(k, e.cfg.Sampling.IdentifierPrefixMax, func(off int) { add(off, "identifier_partial", "") })
			}
			if sub, ok := facts.funcLits[k.start]; ok && k.t == token.FUNC {
				add(k.start, "func_literal", sub)
			}
			if kind, ok := facts.logTemplate[k.start]; ok && k.t == token.STRING {
				add(k.start, "log_message", "template_start")
				if b[k.start] == '"' && k.end-k.start >= 2 {
					c := candidate{off: k.start + 1, kind: "log_message", subkind: "template_body"}
					cands[c.off] = c
				}
				_ = kind
			}
			e1 := k.end
			switch {
			case k.t == token.PERIOD && next != nil && (next.t == token.IDENT || next.t == token.LPAREN):
				sub := "value"
				if next.t == token.LPAREN {
					sub = "type_assertion"
				} else if prev != nil && prev.t == token.IDENT && facts.importNames[prev.lit] && (ti < 2 || toks[ti-2].t != token.PERIOD) {
					sub = "package"
				} else if prev != nil && prev.t == token.RPAREN {
					sub = "call_result"
				}
				add(e1, "member_access", sub)
			case k.t == token.LPAREN && facts.callLparen[k.start]:
				if ec, ok := facts.errCtor[k.start]; ok {
					add(e1, "error_handling", "err_construct:"+ec)
				} else {
					add(e1, "argument_list", "call_open")
				}
			case k.t == token.LPAREN && facts.paramsLparen[k.start]:
				add(e1, "argument_list", "params_open")
			case k.t == token.COMMA:
				switch k.ctx {
				case "call":
					add(afterSpace(e1), "argument_list", "call_next")
				case "params":
					add(afterSpace(e1), "argument_list", "params_next")
				case "composite":
					add(afterSpace(e1), "composite_literal", "next")
				}
			case k.t == token.LBRACE && facts.compositeLb[k.start]:
				add(e1, "composite_literal", "open")
			case k.t == token.COLON && k.ctx == "composite":
				add(afterSpace(e1), "composite_literal", "key_value")
			case controlFlow[k.t] && e1 < le && b[e1] == ' ':
				if facts.errIfCond[afterSpace(e1)] {
					add(afterSpace(e1), "error_handling", "if_err_cond")
				} else {
					add(afterSpace(e1), "control_flow", k.t.String())
				}
			case afterKeyword[k.t] && e1 < le && b[e1] == ' ':
				if k.t == token.RETURN && insideErrBody(facts, k.start) {
					add(afterSpace(e1), "error_handling", "err_return")
				} else {
					add(afterSpace(e1), "after_keyword", k.t.String())
				}
			case afterOperator[k.t]:
				add(afterSpace(e1), "after_operator", k.t.String())
			}
			add(e1, "token_boundary", "")
		}
		// negative-stratum probes inside literals/comments on this line (counted, never silently dropped)
		for _, ti := range lits {
			k := toks[ti]
			if k.end-k.start >= 2 && k.start+1 <= le {
				if _, ok := cands[k.start+1]; !ok {
					cands[k.start+1] = candidate{off: k.start + 1, kind: "token_boundary", probe: true}
				}
			}
		}
		prevLineEndsWithDot = toks[code[len(code)-1]].t == token.PERIOD

		offs := make([]int, 0, len(cands))
		for o := range cands {
			offs = append(offs, o)
		}
		sort.Ints(offs)
		for _, o := range offs {
			c := cands[o]
			if !c.probe {
				cnt("candidates."+c.kind, 1)
			}
			reason := ""
			if t := tokenAt(o); t != nil && !(c.kind == "log_message" && c.subkind == "template_body") {
				switch {
				case t.t == token.COMMENT:
					reason = "in_comment"
				case t.t == token.STRING && b[t.start] == '`':
					reason = "in_raw_string"
				case t.t == token.STRING:
					reason = "in_string"
				case t.t == token.CHAR:
					reason = "in_rune_literal"
				case c.kind != "identifier_partial":
					reason = "caret_inside_token"
				}
			}
			te := TrimRightHSpace(b, o, le)
			if reason == "" && te <= o {
				reason = "empty_target"
			}
			if reason == "" {
				for _, m := range multi {
					if m.start < te && m.end > o {
						if m.raw {
							reason = "raw_multiline_string"
						} else {
							reason = "multiline_comment"
						}
						break
					}
				}
			}
			if reason == "" {
				n := UTF16Len(string(b[o:te]))
				if n > e.cfg.Sampling.MaxTargetChars {
					reason = "target_too_long"
				} else if n < e.cfg.Sampling.MinTargetChars {
					reason = "empty_target"
				}
			}
			if reason == "" && lineSecret {
				reason = "secret_detected"
			}
			if reason == "" && c.probe {
				reason = "probe_unclassified"
			}
			if reason != "" {
				addExclusion(l, o, reason, c.kind)
				continue
			}
			cnt("eligible."+c.kind, 1)
			w := weights[c.kind]
			target := b[o:te]
			trivial := isTrivial(target)
			if trivial {
				w *= e.cfg.Sampling.TrivialTargetKeepProbability
			}
			u := Uniform(strconv.FormatInt(e.cfg.Seed, 10), "caret", src.Sha256, strconv.Itoa(o))
			if w <= 0 || u >= w {
				continue
			}
			elig = append(elig, eligible{c: c, line: l, targetEnd: te, trivial: trivial, u: u, score: u / w})
		}
	}

	// per-line cap, then per-file cap (lowest score wins), output in source order
	byLine := map[int][]int{}
	for i, x := range elig {
		byLine[x.line] = append(byLine[x.line], i)
	}
	var kept []int
	for _, idx := range byLine {
		sort.Slice(idx, func(a, b int) bool {
			return lessScore(elig[idx[a]].score, elig[idx[a]].c.off, elig[idx[b]].score, elig[idx[b]].c.off)
		})
		if n := e.cfg.Sampling.MaxSamplesPerLine; n > 0 && len(idx) > n {
			idx = idx[:n]
		}
		kept = append(kept, idx...)
	}
	sort.Slice(kept, func(a, b int) bool {
		return lessScore(elig[kept[a]].score, elig[kept[a]].c.off, elig[kept[b]].score, elig[kept[b]].c.off)
	})
	if n := e.cfg.Sampling.MaxSamplesPerFile; n > 0 && len(kept) > n {
		cnt("sampling.file_cap_dropped", len(kept)-n)
		kept = kept[:n]
	}
	sort.Slice(kept, func(a, b int) bool { return elig[kept[a]].c.off < elig[kept[b]].c.off })

	for _, i := range kept {
		x := elig[i]
		s := e.materialize(af, x.c, x.line, x.targetEnd, x.trivial, syntaxErrors, facts, toks)
		res.Samples = append(res.Samples, s)
		ls := src.LineStarts[x.line]
		trimStart := ls
		for trimStart < x.c.off && isHSpace(b[trimStart]) {
			trimStart++
		}
		lineTrim := strings.TrimSpace(string(b[ls:src.LineContentEnd[x.line]]))
		res.DedupKeys = append(res.DedupKeys, lineTrim+"\x1f"+strconv.Itoa(x.c.off-trimStart))
		cnt("sampled."+x.c.kind, 1)
	}
	if e.writeCorpus {
		res.Corpus = e.corpus(af)
	}
	return res
}

func lessScore(sa float64, oa int, sb float64, ob int) bool {
	if sa != sb {
		return sa < sb
	}
	return oa < ob
}

func insideErrBody(f *astFacts, off int) bool {
	for _, s := range f.spans {
		if s.tag == "error_handling" && s.start < off && off < s.end {
			return true
		}
	}
	return false
}

func identPartials(k tok, prefixMax int, add func(int)) {
	lit := k.lit
	n := utf8.RuneCountInString(lit)
	if n < 2 {
		return
	}
	offs := map[int]bool{}
	i, prevR := 0, rune(0)
	for bi, r := range lit {
		if i >= 1 && i < n {
			if i <= prefixMax {
				offs[bi] = true
			}
			if (unicode.IsUpper(r) && unicode.IsLower(prevR)) || prevR == '_' {
				offs[bi] = true
			}
		}
		prevR = r
		i++
	}
	keys := make([]int, 0, len(offs))
	for o := range offs {
		keys = append(keys, o)
	}
	sort.Ints(keys)
	for _, o := range keys {
		add(k.start + o)
	}
}

func isTrivial(t []byte) bool {
	for _, c := range t {
		if !strings.ContainsRune("{}()[];,", rune(c)) {
			return false
		}
	}
	return true
}

func (e *Extractor) materialize(af *AcceptedFile, c candidate, line, te int, trivial, syntaxErrors bool, facts *astFacts, toks []tok) *Sample {
	src := af.Src
	b := src.Original
	ls, le, lb := src.LineStarts[line], src.LineContentEnd[line], src.LineBreakEnd[line]
	caretU := src.UTF16(c.off)
	teU := src.UTF16(te)
	lcs := src.BackByUTF16(c.off, e.cfg.Context.LeftChars, src.BOMLen)
	rce := src.ForwardByUTF16(te, e.cfg.Context.RightChars)
	eol := "EOF"
	if lb > le {
		if b[le] == '\r' {
			eol = "CRLF"
		} else {
			eol = "LF"
		}
	}
	ind := ls
	for ind < le && isHSpace(b[ind]) {
		ind++
	}
	target := string(b[c.off:te])
	var flags []string
	if trivial {
		flags = append(flags, "trivial_target")
	}
	if isHSpace(b[c.off]) {
		flags = append(flags, "target_starts_with_whitespace")
	}
	if strings.ContainsRune(")]}", rune(b[c.off])) {
		flags = append(flags, "target_starts_with_closer")
	}
	if te < le {
		flags = append(flags, "trailing_whitespace")
	}
	for _, k := range toks {
		if k.t == token.COMMENT && k.start >= c.off && k.start < te {
			flags = append(flags, "target_has_comment")
			break
		}
	}
	if syntaxErrors {
		flags = append(flags, "file_has_syntax_errors")
	}
	for _, r := range target {
		if r > 127 {
			flags = append(flags, "non_ascii_target")
			break
		}
	}
	tagSet := map[string]bool{}
	for _, s := range facts.spans {
		if s.start < c.off && c.off < s.end {
			tagSet[s.tag] = true
		}
	}
	if af.IsTest {
		tagSet["test_code"] = true
	}
	if !af.BuildMatch {
		tagSet["build_excluded"] = true
	}
	if af.Cgo {
		tagSet["cgo"] = true
	}
	if c.kind == "log_message" || facts.logTemplate[c.off] != "" || facts.logTemplate[c.off-1] != "" {
		tagSet["log_call"] = true
	}
	if c.kind == "error_handling" {
		tagSet["error_handling"] = true
	}
	for _, k := range toks {
		if k.start >= ls && k.start < le && (k.t == token.ARROW || k.t == token.CHAN) {
			tagSet["channel"] = true
			break
		}
	}
	tags := make([]string, 0, len(tagSet))
	for t := range tagSet {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	if flags == nil {
		flags = []string{}
	}
	var sub *string
	if c.subkind != "" {
		sub = strp(c.subkind)
	}
	return &Sample{
		SchemaVersion: "flc-sample/v1",
		SampleID:      StableID(e.repo.RepositoryID, af.Rel, src.Sha256, strconv.Itoa(caretU), strconv.Itoa(teU)),
		Language:      "go", RepositoryID: e.repo.RepositoryID, Revision: e.repo.Revision, RelativePath: af.Rel,
		Project: af.Project, PackageName: af.PackageName, IsTest: af.IsTest, SourceSha256: src.Sha256,
		CaretUTF16Offset: caretU, CaretByteOffset: c.off, CaretLine: line,
		CaretColumnUTF16: caretU - src.UTF16(ls), CaretColumnByte: c.off - ls,
		TargetEndUTF16Offset: teU, TargetEndByteOffset: te,
		LineStartUTF16Offset: src.UTF16(ls), LineStartByteOffset: ls,
		LineEndUTF16Offset: src.UTF16(le), LineEndByteOffset: le,
		CaretKind: c.kind, CaretSubkind: sub, Tags: tags,
		LeftContextStartUTF16Offset: src.UTF16(lcs), LeftContextStartByteOffset: lcs,
		LeftContextTruncated: lcs > src.BOMLen, LeftContext: string(b[lcs:c.off]),
		TargetText: target, RightContext: string(b[te:rce]),
		RightContextEndUTF16Offset: src.UTF16(rce), RightContextEndByteOffset: rce,
		RightContextTruncated: rce < len(b),
		EndOfLine:             eol, Indentation: string(b[ls:ind]), QualityFlags: flags,
		Split: e.split, SplitGroup: e.group,
		BuildConstraint: af.BuildConstraint, BuildMatch: af.BuildMatch,
		SemanticStatus: "not_attempted", SemanticReason: strp("not_requested"),
		ConfigVersion: e.cfg.ConfigVersion, ConfigSha256: e.cfgSha, Generator: Generator,
	}
}

func (e *Extractor) corpus(af *AcceptedFile) *CorpusFile {
	src := af.Src
	b := src.Original
	nl := "none"
	crlf, lf := 0, 0
	for i := range src.LineStarts {
		if src.LineBreakEnd[i] > src.LineContentEnd[i] {
			if b[src.LineContentEnd[i]] == '\r' {
				crlf++
			} else {
				lf++
			}
		}
	}
	switch {
	case crlf > 0 && lf > 0:
		nl = "mixed"
	case crlf > 0:
		nl = "crlf"
	case lf > 0:
		nl = "lf"
	}
	var lr *string
	if e.repo.LicenseReason != "" {
		lr = strp(e.repo.LicenseReason)
	}
	return &CorpusFile{SchemaVersion: "corpus-file/v1", RepositoryID: e.repo.RepositoryID, Revision: e.repo.Revision,
		RelativePath: af.Rel, Language: "go", Sha256: src.Sha256, Bytes: len(b), License: e.repo.License,
		LicenseReason: lr, Encoding: "utf-8", HasBOM: src.BOMLen > 0, NewlineStyle: nl, Project: af.Project,
		PackageName: af.PackageName, IsTest: af.IsTest, Split: e.split, Lines: src.NumLines(),
		BuildConstraint: af.BuildConstraint, BuildMatch: af.BuildMatch, Content: string(src.Text()),
		ContentNormalized: false}
}
