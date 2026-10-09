package render

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goflc/internal/flc"
	"goflc/internal/semantic"
)

func sp(s string) *string { return &s }

func sample(left, target string) *flc.Sample {
	return &flc.Sample{SampleID: "0123456789abcdef0123456789abcdef", RelativePath: "pkg/a.go", Split: "train",
		CaretKind: "member_access", SemanticStatus: "resolved", LeftContext: left, TargetText: target,
		RightContext: "\n}\n"}
}

func TestPromptEndsAtCaretAndHidesTarget(t *testing.T) {
	left := "package a\n\nfunc f(s *Store) {\n\tu, err := s."
	target := "GetUser(ctx, secretIdentifierOnlyInTarget)"
	s := sample(left, target)
	r := Render(s, nil, DefaultOptions())
	if !strings.HasSuffix(r.Prompt, left+Complete) {
		t.Fatalf("prompt must end with the left context + %s:\n%s", Complete, r.Prompt)
	}
	if strings.Contains(r.Prompt, "secretIdentifierOnlyInTarget") || strings.Contains(r.Prompt, "GetUser") {
		t.Fatal("target text leaked into the prompt")
	}
	if strings.Contains(r.Prompt, s.RightContext[1:]) && strings.Contains(r.Prompt, "\n}\n"+Complete) {
		t.Fatal("right context rendered")
	}
	if r.Completion != target+Eol {
		t.Fatalf("completion %q", r.Completion)
	}
	if !strings.HasPrefix(r.Prompt, Lang+Path+"pkg/a.go\n"+Code+"\n") || r.HasSemantic {
		t.Fatalf("syntax-only prompt layout wrong:\n%s", r.Prompt)
	}
}

func TestCodeCutOnLineBoundaryFromTheLeft(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		b.WriteString("\tx := compute(😀, 12345)\n")
	}
	b.WriteString("\treturn x.")
	left := b.String()
	o := DefaultOptions()
	o.MaxCodeChars = 500
	r := Render(sample(left, "Value()"), nil, o)
	_, code, _ := strings.Cut(r.Prompt, Code+"\n")
	code = strings.TrimSuffix(code, Complete)
	if !r.CodeTruncated || u16(code) > 500 {
		t.Fatalf("budget: %d units, truncated=%v", u16(code), r.CodeTruncated)
	}
	if !strings.HasSuffix(left, code) || !strings.HasPrefix(code, "\tx := ") {
		t.Fatalf("code must be a suffix of the left context starting at a line boundary: %q", code[:40])
	}
}

func rec() *semantic.Record {
	f := func(n, k, t string) semantic.Fact { return semantic.Fact{Name: n, Kind: k, Type: sp(t)} }
	m := func(n, sig string) semantic.Fact { return semantic.Fact{Name: n, Kind: "method", Signature: sp(sig)} }
	idx := 1
	var members []semantic.Fact
	for i := 0; i < 60; i++ {
		members = append(members, m("Method"+strings.Repeat("x", i%7)+string(rune('A'+i%26)), "M(a int, b string) (int, error)"))
	}
	return &semantic.Record{Status: "resolved", VisibilityPolicy: "editor_snapshot",
		ReturnType: sp("(*User, error)"), ExpectedType: sp("time.Duration"),
		Parameters:      []semantic.Fact{f("ctx", "parameter", "context.Context"), f("id", "parameter", "int")},
		Locals:          []semantic.Fact{f("u", "local", "*User")},
		ReceiverMembers: []semantic.Fact{f("store", "field", "*store.Store"), m("Rename", "Rename(ctx context.Context, id int) (*User, error)")},
		ReceiverType:    sp("time"), ReceiverKind: sp("package"), Members: members,
		CallName: sp("svc.take"), CallSignature: sp("(n int, d time.Duration)"), ArgumentIndex: &idx, ParameterName: sp("d"),
		ContextTypes: []semantic.TypeContract{{Name: "User", Kind: "struct", Members: []semantic.Fact{f("ID", "field", "int")}, TotalMembers: 1}},
	}
}

func TestSemanticBlockLinesAndOrder(t *testing.T) {
	r := Render(sample("\tsvc.take(1, ", "time.Second)"), rec(), DefaultOptions())
	if !r.HasSemantic {
		t.Fatal("expected semantic block")
	}
	head, _, _ := strings.Cut(r.Prompt, Code)
	want := []string{"RET (*User, error)", "EXPECT time.Duration", "ARG ctx:context.Context id:int", "LOCAL u:*User",
		"FIELD store:*store.Store", "METHOD Rename(ctx context.Context, id int)->(*User, error)", "RECV package time",
		"MEMBER ", "TYPE User: ID:int", "CALL svc.take(n int, d time.Duration) @1 d:time.Duration"}
	last := -1
	for _, w := range want {
		i := strings.Index(head, w)
		if i < 0 {
			t.Fatalf("missing %q in\n%s", w, head)
		}
		if i < last {
			t.Fatalf("canonical order broken at %q", w)
		}
		last = i
	}
	if r.SemanticItemsDropped == 0 {
		t.Fatal("MEMBER list above 24 items must be trimmed")
	}
}

func TestSemanticBudgetPriority(t *testing.T) {
	o := DefaultOptions()
	o.MaxSemanticChars = 120
	text, dropped := RenderSemantic(rec(), o)
	if u16(text) > 120 {
		t.Fatalf("budget exceeded: %d", u16(text))
	}
	// highest priorities survive a tight budget; the lowest (METHOD) is dropped
	if !strings.Contains(text, "EXPECT time.Duration") || !strings.Contains(text, "RECV package time") {
		t.Fatalf("priority lines missing:\n%s", text)
	}
	if strings.Contains(text, "METHOD ") || dropped == 0 {
		t.Fatalf("low-priority lines should be dropped:\n%s", text)
	}
}

func TestFallbackRecordsRenderWithoutSemantic(t *testing.T) {
	r := rec()
	r.Status = "failed"
	out := Render(sample("\tx.", "Y()"), r, DefaultOptions())
	if out.HasSemantic || strings.Contains(out.Prompt, Sem) {
		t.Fatal("failed records must not render facts")
	}
}

func TestCompactSig(t *testing.T) {
	for in, want := range map[string]string{
		"Get(ctx context.Context, id int) (*User, error)": "Get(ctx context.Context, id int)->(*User, error)",
		"Close()":                                "Close()",
		"Map[T, U any](xs []T, f func(T) U) []U": "Map[T, U any](xs []T, f func(T) U)->[]U",
		"Do(f func(int) error) error":            "Do(f func(int) error)->error",
	} {
		if got := CompactSig(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

// End-to-end on the fixture: every prompt ends at the caret; completion = target + <|eol|>.
func TestDatasetRender(t *testing.T) {
	repo, _ := filepath.Abs("../../fixtures/basic")
	cfg := flc.DefaultConfig()
	cfg.Semantic.Mode = "best_effort"
	cp := filepath.Join(t.TempDir(), "c.json")
	b, _ := json.Marshal(cfg)
	os.WriteFile(cp, b, 0o644)
	out := filepath.Join(t.TempDir(), "ds")
	if _, err := flc.Run(flc.RunOptions{Repo: repo, Out: out, ConfigPath: cp, RepositoryID: "fixture/basic", Workers: 2,
		SemanticHook: semantic.Run}); err != nil {
		t.Fatal(err)
	}
	pd := filepath.Join(t.TempDir(), "prompts")
	sum, err := Dataset(out, pd, DefaultOptions(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Records == 0 || sum.WithSemantic == 0 {
		t.Fatalf("summary %+v", sum)
	}
	samples := map[string]flc.Sample{}
	f, _ := os.Open(filepath.Join(out, "samples.jsonl"))
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		var s flc.Sample
		json.Unmarshal(sc.Bytes(), &s)
		samples[s.SampleID] = s
	}
	f.Close()
	pf, _ := os.Open(filepath.Join(pd, "prompts.train.jsonl"))
	defer pf.Close()
	sc = bufio.NewScanner(pf)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	n := 0
	for sc.Scan() {
		var r TrainingRecord
		json.Unmarshal(sc.Bytes(), &r)
		s := samples[r.SampleID]
		if !strings.HasSuffix(r.Prompt, Complete) || r.Completion != s.TargetText+Eol {
			t.Fatalf("bad record %s", r.SampleID)
		}
		_, code, _ := strings.Cut(r.Prompt, Code+"\n")
		if !strings.HasSuffix(s.LeftContext, strings.TrimSuffix(code, Complete)) {
			t.Fatalf("code window is not a suffix of left_context: %s", r.SampleID)
		}
		n++
	}
	if n != sum.Records {
		t.Fatalf("records %d vs %d", n, sum.Records)
	}
	if _, err := os.Stat(filepath.Join(pd, "preview.md")); err != nil {
		t.Fatal("no preview")
	}
}
