// Package render serializes canonical samples (+ the editor_snapshot semantic sidecar) into flc-prompt/v2 training
// records, the Go counterpart of the C# PromptRenderer (docs/PROMPT_FORMAT.md):
//
//	<|go|><|path|>internal/x/y.go\n
//	<|sem|>\nRET ...\nEXPECT ...\n...           (omitted when no resolved semantic facts)
//	<|code|>\n...left context ending at the caret...<|complete|>
//	completion = target_text + <|eol|>
//
// Angle-bracket markers are meant to become dedicated special-token ids. The right context is never rendered, and
// nothing is read from the hidden target except to form the completion.
package render

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"goflc/internal/flc"
	"goflc/internal/semantic"
)

const (
	FormatV1 = "flc-prompt/v1"
	FormatV2 = "flc-prompt/v2" // v1 + TYPE lines

	Lang     = "<|go|>"
	Path     = "<|path|>"
	Sem      = "<|sem|>"
	Code     = "<|code|>"
	Complete = "<|complete|>"
	Eol      = "<|eol|>"
)

// SpecialTokens are the markers that must map to dedicated token ids.
var SpecialTokens = []string{Lang, Path, Sem, Code, Complete, Eol, "<|end_completion|>", "<|eos|>"}

// Options are the serialization budgets; sizes are UTF-16 code units until a tokenizer is pinned.
type Options struct {
	Format           string `json:"format"`
	IncludeTypes     bool   `json:"include_types"`
	MaxCodeChars     int    `json:"max_code_chars"`
	MaxSemanticChars int    `json:"max_semantic_chars"`
	IncludePath      bool   `json:"include_path"`
	Policy           string `json:"policy"`
	MaxItemsPerLine  int    `json:"max_items_per_line"`
}

func DefaultOptions() Options {
	return Options{Format: FormatV2, IncludeTypes: true, MaxCodeChars: 4000, MaxSemanticChars: 1500, IncludePath: true,
		Policy: "editor_snapshot", MaxItemsPerLine: 24}
}

// TrainingRecord is flc-train/v1: loss applies to Completion only.
type TrainingRecord struct {
	SchemaVersion        string `json:"schema_version"`
	SampleID             string `json:"sample_id"`
	Split                string `json:"split"`
	PromptFormat         string `json:"prompt_format"`
	CaretKind            string `json:"caret_kind"`
	SemanticStatus       string `json:"semantic_status"`
	HasSemantic          bool   `json:"has_semantic"`
	Prompt               string `json:"prompt"`
	Completion           string `json:"completion"`
	CodeChars            int    `json:"code_chars"`
	CodeTruncated        bool   `json:"code_truncated"`
	SemanticChars        int    `json:"semantic_chars"`
	SemanticItemsDropped int    `json:"semantic_items_dropped"`
}

func u16(s string) int { return len(utf16.Encode([]rune(s))) }

// Render builds one training record. sem may be nil.
func Render(s *flc.Sample, sem *semantic.Record, o Options) TrainingRecord {
	var b strings.Builder
	b.WriteString(Lang)
	if o.IncludePath {
		b.WriteString(Path)
		b.WriteString(s.RelativePath)
	}
	b.WriteByte('\n')
	semText, dropped := "", 0
	if sem != nil && (sem.Status == "resolved" || sem.Status == "partially_resolved") {
		semText, dropped = RenderSemantic(sem, o)
	}
	if semText != "" {
		b.WriteString(Sem)
		b.WriteByte('\n')
		b.WriteString(semText)
	}
	left, truncated := CutCode(s.LeftContext, o.MaxCodeChars)
	truncated = truncated || s.LeftContextTruncated
	b.WriteString(Code)
	b.WriteByte('\n')
	b.WriteString(left)
	b.WriteString(Complete)
	return TrainingRecord{SchemaVersion: "flc-train/v1", SampleID: s.SampleID, Split: s.Split, PromptFormat: o.Format,
		CaretKind: s.CaretKind, SemanticStatus: s.SemanticStatus, HasSemantic: semText != "", Prompt: b.String(),
		Completion: s.TargetText + Eol, CodeChars: u16(left), CodeTruncated: truncated, SemanticChars: u16(semText),
		SemanticItemsDropped: dropped}
}

// CutCode keeps the most recent maxChars UTF-16 units of the left context, cut on a line boundary from the left
// (never on the caret side, never inside a rune).
func CutCode(left string, maxChars int) (string, bool) {
	if maxChars <= 0 || u16(left) <= maxChars {
		return left, false
	}
	// walk back from the end until the budget is used
	n, i := 0, len(left)
	for i > 0 {
		r, size := utf8.DecodeLastRuneInString(left[:i])
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if n+w > maxChars {
			break
		}
		n += w
		i -= size
	}
	cut := i
	if nl := strings.IndexByte(left[cut:], '\n'); nl >= 0 && cut+nl < len(left)-1 {
		cut += nl + 1
	}
	return left[cut:], true
}

type line struct {
	order    float64
	priority int
	key      string
	items    []string
	sep      string
}

// RenderSemantic renders the semantic block with a hard UTF-16 char budget. Lines are admitted by priority
// (EXPECT, RECV, CALL, ARG, LOCAL, RET, MEMBER, FIELD, TYPE, METHOD); list items are trimmed from the end. Output
// order is canonical regardless of priority.
func RenderSemantic(r *semantic.Record, o Options) (string, int) {
	var lines []line
	add := func(order float64, prio int, key string, items []string, sep string) {
		var list []string
		for _, x := range items {
			if x != "" {
				list = append(list, x)
			}
		}
		if len(list) > 0 {
			lines = append(lines, line{order, prio, key, list, sep})
		}
	}
	one := func(p *string) []string {
		if p == nil {
			return nil
		}
		return []string{*p}
	}
	add(0, 5, "RET", one(r.ReturnType), " ")
	add(1, 0, "EXPECT", one(r.ExpectedType), " ")
	add(2, 3, "ARG", typed(r.Parameters), " ")
	add(3, 4, "LOCAL", typed(r.Locals), " ")
	var fields, methods []semantic.Fact
	for _, m := range r.ReceiverMembers {
		if m.Kind == "method" {
			methods = append(methods, m)
		} else {
			fields = append(fields, m)
		}
	}
	add(4, 7, "FIELD", typed(fields), " ")
	add(6, 9, "METHOD", compactAll(methods), "; ")
	if r.ReceiverType != nil {
		recv := *r.ReceiverType
		if r.ReceiverKind != nil && (*r.ReceiverKind == "package" || *r.ReceiverKind == "type") {
			recv = *r.ReceiverKind + " " + recv
		}
		add(7, 1, "RECV", []string{recv}, " ")
	}
	add(8, 6, "MEMBER", compactAll(r.Members), "; ")
	if o.IncludeTypes {
		for i, t := range r.ContextTypes {
			add(8.5+float64(i)*0.01, 8, "TYPE "+t.Name+":", compactAll(t.Members), "; ")
		}
	}
	if r.CallSignature != nil {
		c := ""
		if r.CallName != nil {
			c = CompactFunc(*r.CallName, *r.CallSignature)
		} else {
			c = *r.CallSignature
		}
		if r.ArgumentIndex != nil {
			c += fmt.Sprintf(" @%d", *r.ArgumentIndex)
		}
		if r.ParameterName != nil {
			c += " " + *r.ParameterName
			if r.ExpectedType != nil {
				c += ":" + *r.ExpectedType
			}
		}
		add(9, 2, "CALL", []string{c}, " | ")
	}

	budget, dropped := o.MaxSemanticChars, 0
	type adm struct {
		order float64
		text  string
	}
	var admitted []adm
	byPrio := append([]line{}, lines...)
	sort.SliceStable(byPrio, func(i, j int) bool { return byPrio[i].priority < byPrio[j].priority })
	for _, l := range byPrio {
		items := l.items
		if o.MaxItemsPerLine > 0 && len(items) > o.MaxItemsPerLine {
			dropped += len(items) - o.MaxItemsPerLine
			items = items[:o.MaxItemsPerLine]
		}
		text := func() string { return l.key + " " + strings.Join(items, l.sep) + "\n" }
		for len(items) > 0 && u16(text()) > budget {
			items = items[:len(items)-1]
			dropped++
		}
		if len(items) == 0 {
			continue
		}
		t := text()
		budget -= u16(t)
		admitted = append(admitted, adm{l.order, t})
	}
	sort.SliceStable(admitted, func(i, j int) bool { return admitted[i].order < admitted[j].order })
	var out strings.Builder
	for _, a := range admitted {
		out.WriteString(a.text)
	}
	return out.String(), dropped
}

func typed(fs []semantic.Fact) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		if f.Type != nil {
			out = append(out, f.Name+":"+*f.Type)
		} else {
			out = append(out, f.Name)
		}
	}
	return out
}

func compactAll(fs []semantic.Fact) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, compact(f))
	}
	return out
}

func compact(f semantic.Fact) string {
	switch {
	case f.Signature != nil:
		return CompactSig(*f.Signature)
	case f.Kind == "type":
		return f.Name
	case f.Type != nil:
		return f.Name + ":" + *f.Type
	default:
		return f.Name
	}
}

// CompactSig turns a Go signature "Name(params) results" into "Name(params)->results" (no results: "Name(params)").
func CompactSig(sig string) string {
	open := strings.IndexByte(sig, '(')
	if open < 0 {
		return sig
	}
	// type parameter list "Name[T any](...)": the parameter list starts after the matching ']'
	if br := strings.IndexByte(sig, '['); br >= 0 && br < open {
		if end := matching(sig, br, '[', ']'); end > 0 {
			if p := strings.IndexByte(sig[end:], '('); p >= 0 {
				open = end + p
			}
		}
	}
	close := matching(sig, open, '(', ')')
	if close < 0 {
		return sig
	}
	res := strings.TrimSpace(sig[close+1:])
	if res == "" {
		return sig[:close+1]
	}
	return sig[:close+1] + "->" + res
}

// CompactFunc renders a callee name with an anonymous signature "(params) results" as "name(params)->results".
func CompactFunc(name, sig string) string {
	if strings.HasPrefix(sig, "[") || strings.HasPrefix(sig, "(") {
		return CompactSig(name + sig)
	}
	return name + " " + sig
}

func matching(s string, i int, open, close byte) int {
	d := 0
	for j := i; j < len(s); j++ {
		switch s[j] {
		case open:
			d++
		case close:
			d--
			if d == 0 {
				return j
			}
		}
	}
	return -1
}

// Summary is written to render-summary.json.
type Summary struct {
	Format        string         `json:"prompt_format"`
	Records       int            `json:"records"`
	WithSemantic  int            `json:"with_semantic"`
	CodeTruncated int            `json:"code_truncated"`
	ItemsDropped  int            `json:"semantic_items_dropped"`
	BySplit       map[string]int `json:"by_split"`
	Options       Options        `json:"options"`
}

// Dataset renders a dataset directory into outDir/prompts.<split>.jsonl (+ render-summary.json, preview.md).
func Dataset(dataset, outDir string, o Options, preview int) (*Summary, error) {
	sems := map[string]*semantic.Record{}
	if rc, err := flc.OpenJSONL(filepath.Join(dataset, "semantic.jsonl")); err == nil {
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 1<<20), 1<<28)
		for sc.Scan() {
			var r semantic.Record
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				rc.Close()
				return nil, err
			}
			if r.VisibilityPolicy == o.Policy {
				rr := r
				sems[r.SampleID] = &rr
			}
		}
		rc.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	rc, err := flc.OpenJSONL(filepath.Join(dataset, "samples.jsonl"))
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	writers := map[string]*flc.JSONLWriter{}
	sum := &Summary{Format: o.Format, BySplit: map[string]int{}, Options: o}
	type pv struct {
		key float64
		s   flc.Sample
		t   TrainingRecord
	}
	var previews []pv
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 1<<20), 1<<28)
	for sc.Scan() {
		var s flc.Sample
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			return nil, err
		}
		t := Render(&s, sems[s.SampleID], o)
		w, ok := writers[s.Split]
		if !ok {
			if w, err = flc.NewJSONLWriter(filepath.Join(outDir, "prompts."+s.Split+".jsonl"), false); err != nil {
				return nil, err
			}
			writers[s.Split] = w
		}
		if err := w.Write(t); err != nil {
			return nil, err
		}
		sum.Records++
		sum.BySplit[s.Split]++
		if t.HasSemantic {
			sum.WithSemantic++
		}
		if t.CodeTruncated {
			sum.CodeTruncated++
		}
		sum.ItemsDropped += t.SemanticItemsDropped
		if preview > 0 {
			k := flc.Uniform("preview", s.SampleID)
			if t.HasSemantic {
				k -= 1 // semantic examples first
			}
			previews = append(previews, pv{k, s, t})
			if len(previews) > preview*4 {
				sort.Slice(previews, func(i, j int) bool { return previews[i].key < previews[j].key })
				previews = previews[:preview]
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for _, w := range writers {
		if err := w.Close(); err != nil {
			return nil, err
		}
	}
	b, _ := json.MarshalIndent(sum, "", "  ")
	if err := os.WriteFile(filepath.Join(outDir, "render-summary.json"), append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	if preview > 0 {
		sort.Slice(previews, func(i, j int) bool { return previews[i].key < previews[j].key })
		if len(previews) > preview {
			previews = previews[:preview]
		}
		var md strings.Builder
		fmt.Fprintf(&md, "# %s preview\n\nSpecial tokens are shown literally. Loss applies to COMPLETION only.\n\n", o.Format)
		for _, p := range previews {
			fmt.Fprintf(&md, "## %s `%s:%d` (%s)\n\n```text\n%s\n```\n\nCOMPLETION: `%s`\n\n", p.s.CaretKind,
				p.s.RelativePath, p.s.CaretLine+1, p.s.SemanticStatus, tail(p.t.Prompt, 40), p.t.Completion)
		}
		if err := os.WriteFile(filepath.Join(outDir, "preview.md"), []byte(md.String()), 0o644); err != nil {
			return nil, err
		}
	}
	return sum, nil
}

// tail keeps the header/semantic block and the last n code lines of a prompt (preview readability only).
func tail(prompt string, n int) string {
	head, code, ok := strings.Cut(prompt, Code+"\n")
	if !ok {
		return prompt
	}
	ls := strings.Split(code, "\n")
	if len(ls) > n {
		ls = append([]string{"…"}, ls[len(ls)-n:]...)
	}
	return head + Code + "\n" + strings.Join(ls, "\n")
}
