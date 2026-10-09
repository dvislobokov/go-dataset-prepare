package flc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

const mitLicense = `MIT License

Permission is hereby granted, free of charge, to any person obtaining a copy of this software.

The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.
`

func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// allCaretsConfig accepts every eligible candidate (weights 1, no caps) so invariants are checked exhaustively.
func allCaretsConfig(t *testing.T) string {
	t.Helper()
	cfg := DefaultConfig()
	for k := range cfg.Sampling.Weights {
		cfg.Sampling.Weights[k] = 1
	}
	cfg.Sampling.MaxSamplesPerLine = 0
	cfg.Sampling.MaxSamplesPerFile = 0
	cfg.Sampling.TrivialTargetKeepProbability = 1
	cfg.Sampling.DropDuplicateLineTargets = false
	cfg.Context.LeftChars = 40 // force truncation
	cfg.Context.RightChars = 10
	p := filepath.Join(t.TempDir(), "cfg.json")
	b, _ := json.Marshal(cfg)
	os.WriteFile(p, b, 0o644)
	return p
}

func run(t *testing.T, repo, cfgPath string, workers int) (string, []Sample, map[string]any) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	if _, err := Run(RunOptions{Repo: repo, Out: out, ConfigPath: cfgPath, RepositoryID: "test/repo", Workers: workers}); err != nil {
		t.Fatal(err)
	}
	return out, readSamples(t, out), readJSON(t, filepath.Join(out, "summary.json"))
}

func readSamples(t *testing.T, out string) []Sample {
	f, err := os.Open(filepath.Join(out, "samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var ss []Sample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		var s Sample
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		ss = append(ss, s)
	}
	return ss
}

func readJSON(t *testing.T, p string) map[string]any {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func counter(sum map[string]any, k string) int {
	c := sum["counters"].(map[string]any)
	if v, ok := c[k]; ok {
		return int(v.(float64))
	}
	return 0
}

func mustValidate(t *testing.T, out, repo string) *ValidationReport {
	t.Helper()
	rep, err := Validate(out, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("validation failed: %v %v", rep.Failures, rep.Examples)
	}
	return rep
}

// --- text model -------------------------------------------------------------------------------------------------

func TestUTF16RoundTripBOMCRLFSurrogates(t *testing.T) {
	src := "\xEF\xBB\xBFpackage p\r\n\r\nvar π = \"日本😀x\"\t// tab\r\nvar s = `😀`"
	s, err := DecodeSource([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if s.BOMLen != 3 || s.NumLines() != 4 {
		t.Fatalf("bom=%d lines=%d", s.BOMLen, s.NumLines())
	}
	text := string(s.Text())
	u := utf16.Encode([]rune(text))
	// every rune boundary maps to the UTF-16 offset of the decoded (BOM-free) text and back
	for off := s.BOMLen; off <= len(s.Original); {
		want := len(utf16.Encode([]rune(string(s.Original[s.BOMLen:off]))))
		if got := s.UTF16(off); got != want {
			t.Fatalf("UTF16(%d)=%d want %d", off, got, want)
		}
		back, ok := s.ByteOfUTF16(want)
		if !ok || back != off {
			t.Fatalf("ByteOfUTF16(%d)=%d,%v want %d", want, back, ok, off)
		}
		if off == len(s.Original) {
			break
		}
		_, n := utf8.DecodeRune(s.Original[off:])
		off += n
	}
	if s.UTF16(len(s.Original)) != len(u) {
		t.Fatalf("total utf16 %d want %d", s.UTF16(len(s.Original)), len(u))
	}
	// a UTF-16 offset inside a surrogate pair is rejected
	i := strings.Index(text, "😀")
	mid := len(utf16.Encode([]rune(text[:i]))) + 1
	if _, ok := s.ByteOfUTF16(mid); ok {
		t.Fatal("offset inside surrogate pair accepted")
	}
	// CRLF: content end excludes CR
	if s.Original[s.LineContentEnd[0]] != '\r' || s.LineBreakEnd[0] != s.LineContentEnd[0]+2 {
		t.Fatal("CRLF line table wrong")
	}
	// last line without newline ends at EOF
	if s.LineBreakEnd[3] != len(s.Original) || s.LineContentEnd[3] != len(s.Original) {
		t.Fatal("EOF line wrong")
	}
}

func TestEncodingPolicy(t *testing.T) {
	cases := map[string]error{"\xFF\xFEp\x00": ErrUTF16, "package p\x00": ErrNUL, "package \xC3\x28": ErrInvalidUTF8,
		"package p\rvar x = 1\n": ErrLoneCR}
	for in, want := range cases {
		if _, err := DecodeSource([]byte(in)); err != want {
			t.Errorf("%q: got %v want %v", in, err, want)
		}
	}
}

func TestGlob(t *testing.T) {
	g := NewGlobMatcher([]string{"**/vendor/**", "**/*.pb.go", "testdata/**"})
	for p, want := range map[string]bool{"vendor/a/b.go": true, "x/vendor/c.go": true, "a.pb.go": true,
		"x/y/a.pb.go": true, "testdata/a.go": true, "x/testdata/a.go": false, "vendorx/a.go": false} {
		if g.Match(p) != want {
			t.Errorf("%s: want %v", p, want)
		}
	}
}

// --- extraction invariants -------------------------------------------------------------------------------------

var unicodeFile = "package uni\n\nimport \"fmt\"\n\n// Größe is a size.\ntype Größe struct{ Wert int }\n\nfunc 日本(π float64, s string) string {\n\tgrüße := fmt.Sprintf(\"%s → %v 😀\", s, π)\n\tif len(grüße) > 3 {\n\t\treturn grüße + \"😀😀\"\n\t}\n\tx := Größe{Wert: 1}\n\treturn fmt.Sprint(x.Wert)\n}\n"

func TestReconstructionAllCaretsMixedFiles(t *testing.T) {
	crlf := strings.ReplaceAll(unicodeFile, "\n", "\r\n")
	noEOL := strings.TrimRight(unicodeFile, "\n")
	bom := "\xEF\xBB\xBF" + unicodeFile
	repo := writeRepo(t, map[string]string{
		"LICENSE": mitLicense, "go.mod": "module example.com/m\n\ngo 1.22\n",
		"lf.go": unicodeFile, "crlf/crlf.go": crlf, "noeol/noeol.go": noEOL, "bom/bom.go": bom,
		"tabs/t.go": "package tabs\n\nfunc f() {\n\tx :=\t1\t\n\t_ = x   \n}\n",
	})
	out, samples, sum := run(t, repo, allCaretsConfig(t), 1)
	if len(samples) < 200 {
		t.Fatalf("too few samples: %d", len(samples))
	}
	rep := mustValidate(t, out, repo)
	if rep.Checks["reconstruction"] != len(samples) {
		t.Fatal("not all samples checked")
	}
	seen := map[string]bool{}
	for _, s := range samples {
		seen[s.EndOfLine] = true
		if strings.ContainsAny(s.TargetText, "\r\n") {
			t.Fatalf("newline in target %q", s.TargetText)
		}
		if strings.HasSuffix(s.TargetText, " ") || strings.HasSuffix(s.TargetText, "\t") {
			t.Fatalf("trailing whitespace in target %q", s.TargetText)
		}
		// insertion of the target at the caret reproduces the file exactly, no duplicated characters
		b, _ := os.ReadFile(filepath.Join(repo, s.RelativePath))
		if string(b[:s.CaretByteOffset])+s.TargetText+string(b[s.TargetEndByteOffset:]) != string(b) {
			t.Fatal("reconstruction")
		}
		if s.RelativePath == "bom/bom.go" && s.CaretByteOffset-3 < 0 {
			t.Fatal("caret inside BOM")
		}
		if s.RelativePath == "bom/bom.go" && s.CaretUTF16Offset >= s.CaretByteOffset && s.CaretByteOffset > 3 && strings.ContainsAny(s.LeftContext, "ößü日😀") {
			// with multibyte text before the caret, byte offset must exceed the UTF-16 offset (+BOM)
			t.Fatalf("utf16 %d byte %d", s.CaretUTF16Offset, s.CaretByteOffset)
		}
	}
	for _, e := range []string{"LF", "CRLF", "EOF"} {
		if !seen[e] {
			t.Errorf("no sample with end_of_line %s", e)
		}
	}
	if counter(sum, "samples.flag.non_ascii_target") == 0 {
		t.Error("expected non_ascii_target samples")
	}
	// truncated windows are recorded
	trunc := false
	for _, s := range samples {
		trunc = trunc || s.LeftContextTruncated
	}
	if !trunc {
		t.Error("expected truncated left contexts")
	}
}

func TestDeterminismAcrossRunsAndWorkers(t *testing.T) {
	repo := writeRepo(t, map[string]string{"LICENSE": mitLicense, "a.go": unicodeFile,
		"b/b.go": strings.ReplaceAll(unicodeFile, "uni", "b"), "c/c.go": strings.ReplaceAll(unicodeFile, "日本", "f")})
	out1, _, _ := run(t, repo, "", 1)
	out2, _, _ := run(t, repo, "", 4)
	for _, f := range []string{"samples.jsonl", "discovery.jsonl", "exclusions.jsonl", "summary.json"} {
		a, _ := os.ReadFile(filepath.Join(out1, f))
		b, _ := os.ReadFile(filepath.Join(out2, f))
		if !bytes.Equal(a, b) {
			t.Errorf("%s differs between runs", f)
		}
	}
}

func TestStrata(t *testing.T) {
	src := `package s

import (
	"errors"
	"fmt"
	"log/slog"
)

type T struct{ A, B int }

func f(logger *slog.Logger) error {
	v, err := g()
	if err != nil {
		return fmt.Errorf("g failed: %w", err)
	}
	logger.Info("value computed", "v", v)
	go func() {
		fmt.Println(v)
	}()
	t := T{A: 1, B: 2}
	for i := 0; i < t.A; i++ {
		v += i
	}
	if v > 10 {
		return errors.New("too big")
	}
	return nil
}

func g() (int, error) { return 1, nil }
`
	repo := writeRepo(t, map[string]string{"LICENSE": mitLicense, "s.go": src})
	_, samples, sum := run(t, repo, allCaretsConfig(t), 1)
	kinds := map[string]map[string]bool{}
	for _, s := range samples {
		if kinds[s.CaretKind] == nil {
			kinds[s.CaretKind] = map[string]bool{}
		}
		sub := ""
		if s.CaretSubkind != nil {
			sub = *s.CaretSubkind
		}
		kinds[s.CaretKind][sub] = true
	}
	want := map[string][]string{
		"error_handling":    {"if_err_check", "err_body_return"},
		"log_message":       {"template_start", "template_body"},
		"func_literal":      {"go"},
		"member_access":     {"package", "value"},
		"composite_literal": {"open", "key_value", "next"},
		"control_flow":      {"for", "if"},
		"after_operator":    {":=", "+="},
		"argument_list":     {"call_open"},
		"line_start":        {"short_var_decl", "return", "func_decl"},
	}
	for k, subs := range want {
		for _, sub := range subs {
			if !kinds[k][sub] {
				t.Errorf("missing stratum %s/%s (have %v)", k, sub, kinds[k])
			}
		}
	}
	if counter(sum, "excluded.in_string") == 0 {
		t.Error("in_string negative stratum not counted")
	}
	// tags
	tags := map[string]bool{}
	for _, s := range samples {
		for _, tg := range s.Tags {
			tags[tg] = true
		}
	}
	for _, tg := range []string{"closure", "goroutine", "error_handling", "log_call", "composite_literal"} {
		if !tags[tg] {
			t.Errorf("missing tag %s", tg)
		}
	}
}

func TestNegativeStrataAndLineExclusions(t *testing.T) {
	src := "//go:build linux\n\n// Package n doc.\npackage n\n\n//go:generate echo hi\nconst q = `a\nb\nc`\n\n/*\nblock\n*/\nvar password = \"s3cr3tVALUE99\"\nvar x = 'r' // trailing comment\n"
	repo := writeRepo(t, map[string]string{"LICENSE": mitLicense, "n.go": src})
	out, _, sum := run(t, repo, allCaretsConfig(t), 1)
	for _, k := range []string{"lines.excluded.build_constraint", "lines.excluded.comment", "lines.excluded.directive",
		"lines.excluded.inside_raw_string", "lines.excluded.inside_block_comment", "excluded.raw_multiline_string",
		"excluded.secret_detected", "excluded.in_rune_literal", "excluded.in_comment"} {
		if counter(sum, k) == 0 {
			t.Errorf("counter %s is zero", k)
		}
	}
	// exclusions.jsonl has audit examples
	b, _ := os.ReadFile(filepath.Join(out, "exclusions.jsonl"))
	if !bytes.Contains(b, []byte(`"reason":"secret_detected"`)) {
		t.Error("no secret_detected exclusion example")
	}
}

func TestDiscoveryReasonsAndLicenseGate(t *testing.T) {
	files := map[string]string{
		"LICENSE":        mitLicense,
		"ok.go":          "package ok\n\nfunc A() int { return 1 }\n",
		"dup.go":         "package ok\n\nfunc A() int { return 1 }\n",
		"gen.go":         "// Code generated by x. DO NOT EDIT.\n\npackage ok\n",
		"loose.go":       "// This file was autogenerated by tool. Do not edit.\npackage ok\n",
		"api.pb.go":      "package ok\n",
		"vendor/x/v.go":  "package x\n",
		"testdata/t.go":  "package t\n",
		"_tools/t.go":    "package t\n",
		"tmpl.go":        "package {{.Name}}\n",
		"key.go":         "package ok\n\nconst k = \"AKIAABCDEFGHIJKLMNOP\"\n",
		"win_windows.go": "package ok\n\nfunc W() {}\n",
		"ign.go":         "//go:build ignore\n\npackage main\n\nfunc main() {}\n",
		"cgo.go":         "package ok\n\n// #include <stdio.h>\nimport \"C\"\n\nfunc Z() {}\n",
		"x_test.go":      "package ok\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n",
		"long/long.go":   "package long\n\nvar s = \"" + strings.Repeat("a", 2100) + "\"\n",
	}
	repo := writeRepo(t, files)
	out, _, sum := run(t, repo, "", 1)
	for reason, n := range map[string]int{"exact_duplicate": 1, "generated_marker": 2, "generated_path": 1,
		"vendored_path": 1, "testdata_path": 1, "go_ignored_dir": 1, "invalid_package_clause": 1, "secret_detected": 1,
		"long_lines": 1} {
		if got := counter(sum, "files.skipped."+reason); got != n {
			t.Errorf("files.skipped.%s = %d want %d", reason, got, n)
		}
	}
	if counter(sum, "files.accepted_build_excluded") != 3 { // windows, ignore, cgo (cgo disabled)
		t.Errorf("accepted_build_excluded = %d", counter(sum, "files.accepted_build_excluded"))
	}
	if counter(sum, "files.accepted_test") != 1 || counter(sum, "files.accepted_cgo") != 1 {
		t.Error("test/cgo accounting")
	}
	// every discovered .go path has a record
	b, _ := os.ReadFile(filepath.Join(out, "discovery.jsonl"))
	if n := bytes.Count(b, []byte("\n")); n != len(files)-1 {
		t.Errorf("discovery records %d want %d", n, len(files)-1)
	}
	// license gate: no license file and no declaration -> nothing accepted
	delete(files, "LICENSE")
	repo2 := writeRepo(t, files)
	_, samples, sum2 := run(t, repo2, "", 1)
	if len(samples) != 0 || counter(sum2, "files.skipped.license_not_allowed") == 0 {
		t.Error("license gate did not block unknown license")
	}
	// conflicting declaration blocks too
	cfgDir := t.TempDir()
	lic, reason, ok := ResolveLicense(&Config{License: LicenseConfig{Allowlist: []string{"MIT"}}}, repo, strp("Apache-2.0"))
	if ok || !strings.HasPrefix(reason, "license_conflict") || *lic != "MIT" {
		t.Errorf("conflict not detected: %v %s %v", lic, reason, ok)
	}
	_ = cfgDir
}

func TestBuildExcludedSkipPolicy(t *testing.T) {
	repo := writeRepo(t, map[string]string{"LICENSE": mitLicense, "a_windows.go": "package a\n\nfunc W() {}\n"})
	cfg := DefaultConfig()
	cfg.Discovery.BuildExcludedPolicy = "skip"
	p := filepath.Join(t.TempDir(), "c.json")
	b, _ := json.Marshal(cfg)
	os.WriteFile(p, b, 0o644)
	_, _, sum := run(t, repo, p, 1)
	if counter(sum, "files.skipped.build_excluded") != 1 {
		t.Error("skip policy")
	}
}

func TestLargeFileBoundedOutput(t *testing.T) {
	var b strings.Builder
	b.WriteString("package big\n\nimport \"fmt\"\n\n")
	for i := 0; b.Len() < 900_000; i++ {
		b.WriteString("func f")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString(strconv.Itoa(i))
		b.WriteString("(a int) int {\n\tif a > 1 {\n\t\tfmt.Println(\"value\", a)\n\t}\n\treturn a + 1\n}\n\n")
	}
	repo := writeRepo(t, map[string]string{"LICENSE": mitLicense, "big.go": b.String()})
	out, samples, sum := run(t, repo, "", 1)
	if len(samples) > DefaultConfig().Sampling.MaxSamplesPerFile {
		t.Fatalf("file cap violated: %d", len(samples))
	}
	if counter(sum, "sampling.file_cap_dropped") == 0 {
		t.Error("expected file cap to apply")
	}
	mustValidate(t, out, repo)
}

func TestOverwriteRefusedAndAtomic(t *testing.T) {
	repo := writeRepo(t, map[string]string{"LICENSE": mitLicense, "a.go": "package a\n\nvar X = 1\n"})
	out := filepath.Join(t.TempDir(), "out")
	if _, err := Run(RunOptions{Repo: repo, Out: out, Workers: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(RunOptions{Repo: repo, Out: out, Workers: 1}); err == nil {
		t.Fatal("overwrite without flag accepted")
	}
	if _, err := Run(RunOptions{Repo: repo, Out: out, Workers: 1, Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(filepath.Dir(out))
	if len(ents) != 1 {
		t.Fatalf("leftover tmp dirs: %v", ents)
	}
	m := readJSON(t, filepath.Join(out, "run-manifest.json"))
	outs := m["outputs"].(map[string]any)
	sb, _ := os.ReadFile(filepath.Join(out, "samples.jsonl"))
	if outs["samples.jsonl"] != Sha256Hex(sb) {
		t.Error("manifest checksum mismatch")
	}
}

func TestValidatorDetectsTampering(t *testing.T) {
	repo := writeRepo(t, map[string]string{"LICENSE": mitLicense, "a.go": unicodeFile})
	out, _, _ := run(t, repo, "", 1)
	p := filepath.Join(out, "samples.jsonl")
	b, _ := os.ReadFile(p)
	lines := bytes.SplitN(b, []byte("\n"), 2)
	var s map[string]any
	json.Unmarshal(lines[0], &s)
	s["target_text"] = s["target_text"].(string) + "x"
	nb, _ := json.Marshal(s)
	os.WriteFile(p, append(append(nb, '\n'), lines[1]...), 0o644)
	rep, err := Validate(out, repo)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK || rep.Failures["reconstruction_bytes"] == 0 {
		t.Fatalf("tampering not detected: %v", rep.Failures)
	}
}
