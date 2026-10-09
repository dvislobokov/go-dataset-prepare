package flc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ValidationReport is written to validation.json by `goflc validate`.
type ValidationReport struct {
	SchemaVersion string         `json:"schema_version"`
	Samples       int            `json:"samples"`
	Semantic      int            `json:"semantic_records"`
	Checks        map[string]int `json:"checks"`
	Failures      map[string]int `json:"failures"`
	Examples      []string       `json:"failure_examples"`
	OK            bool           `json:"ok"`
}

var (
	hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)
	hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex40 = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

var validKinds = map[string]bool{}
var validFlags = map[string]bool{"trivial_target": true, "target_starts_with_whitespace": true,
	"target_starts_with_closer": true, "trailing_whitespace": true, "target_has_comment": true,
	"file_has_syntax_errors": true, "non_ascii_target": true}

func init() {
	for _, k := range caretKinds {
		validKinds[k] = true
	}
}

// requiredSampleFields is the schema's "required" list (checked on the raw JSON so missing keys are caught).
var requiredSampleFields = strings.Fields(`schema_version sample_id language repository_id revision relative_path project
package_name is_test source_sha256 caret_utf16_offset caret_byte_offset caret_line_zero_based
caret_column_utf16_zero_based caret_column_byte_zero_based target_end_utf16_offset target_end_byte_offset
line_start_utf16_offset line_start_byte_offset line_end_utf16_offset line_end_byte_offset caret_kind caret_subkind tags
left_context_start_utf16_offset left_context_start_byte_offset left_context_truncated left_context target_text
right_context right_context_end_utf16_offset right_context_end_byte_offset right_context_truncated end_of_line
indentation quality_flags split split_group build_constraint build_match semantic_status semantic_reason
config_version config_sha256 generator`)

// Validate checks every sample against the repository bytes: exact reconstruction in byte AND UTF-16 coordinates,
// target/line invariants, context windows, deterministic id, structural schema rules; and the semantic sidecar's
// leakage audit when present.
func Validate(dataset, repo string) (*ValidationReport, error) {
	rep := &ValidationReport{SchemaVersion: "validation/v1", Checks: map[string]int{}, Failures: map[string]int{}}
	fail := func(kind, detail string) {
		rep.Failures[kind]++
		if len(rep.Examples) < 50 {
			rep.Examples = append(rep.Examples, kind+": "+detail)
		}
	}
	rc, err := OpenJSONL(filepath.Join(dataset, "samples.jsonl"))
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	cache := map[string]*Source{}
	ids := map[string]bool{}
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 1<<20), 1<<28)
	for sc.Scan() {
		line := sc.Bytes()
		rep.Samples++
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			fail("json", err.Error())
			continue
		}
		for _, f := range requiredSampleFields {
			if _, ok := raw[f]; !ok {
				fail("schema_missing_field", f)
			}
		}
		if len(raw) != len(requiredSampleFields) {
			fail("schema_unexpected_fields", strconv.Itoa(len(raw)))
		}
		var s Sample
		if err := json.Unmarshal(line, &s); err != nil {
			fail("json_types", err.Error())
			continue
		}
		where := s.RelativePath + "@" + strconv.Itoa(s.CaretByteOffset)
		rep.Checks["schema"]++
		switch {
		case s.SchemaVersion != "flc-sample/v1" || s.Language != "go":
			fail("schema_version", where)
		case !hex32.MatchString(s.SampleID) || !hex64.MatchString(s.SourceSha256) || !hex64.MatchString(s.ConfigSha256):
			fail("schema_hash_format", where)
		case s.Revision != nil && !hex40.MatchString(*s.Revision):
			fail("schema_revision", where)
		case !validKinds[s.CaretKind]:
			fail("schema_caret_kind", where)
		case s.EndOfLine != "LF" && s.EndOfLine != "CRLF" && s.EndOfLine != "EOF":
			fail("schema_end_of_line", where)
		case s.Split != "train" && s.Split != "eval" && s.Split != "test" && s.Split != "pilot":
			fail("schema_split", where)
		}
		for _, f := range s.QualityFlags {
			if !validFlags[f] {
				fail("schema_quality_flag", f)
			}
		}
		if ids[s.SampleID] {
			fail("duplicate_sample_id", where)
		}
		ids[s.SampleID] = true

		src, ok := cache[s.RelativePath]
		if !ok {
			b, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(s.RelativePath)))
			if err != nil {
				fail("source_missing", s.RelativePath)
				cache[s.RelativePath] = nil
				continue
			}
			src, err = DecodeSource(b)
			if err != nil {
				fail("source_decode", s.RelativePath)
			}
			if len(cache) > 256 {
				cache = map[string]*Source{}
			}
			cache[s.RelativePath] = src
		}
		if src == nil {
			continue
		}
		if src.Sha256 != s.SourceSha256 {
			fail("source_sha256", where)
			continue
		}
		rep.Checks["source_sha256"]++
		b := src.Original
		c, te := s.CaretByteOffset, s.TargetEndByteOffset
		if c < src.BOMLen || te < c || te > len(b) {
			fail("offset_range", where)
			continue
		}
		// exact reconstruction in bytes
		if string(b[c:te]) != s.TargetText {
			fail("reconstruction_bytes", where)
		}
		recon := string(b[:c]) + s.TargetText + string(b[te:])
		if recon != string(b) {
			fail("reconstruction_full", where)
		}
		rep.Checks["reconstruction"]++
		// UTF-16 coordinates agree with byte coordinates
		if src.UTF16(c) != s.CaretUTF16Offset || src.UTF16(te) != s.TargetEndUTF16Offset {
			fail("utf16_offset", where)
		}
		if bc, ok := src.ByteOfUTF16(s.CaretUTF16Offset); !ok || bc != c {
			fail("utf16_roundtrip", where)
		}
		l := src.LineOf(c)
		ls, le := src.LineStarts[l], src.LineContentEnd[l]
		if l != s.CaretLine || ls != s.LineStartByteOffset || le != s.LineEndByteOffset ||
			s.CaretColumnByte != c-ls || s.CaretColumnUTF16 != s.CaretUTF16Offset-src.UTF16(ls) ||
			s.LineStartUTF16Offset != src.UTF16(ls) || s.LineEndUTF16Offset != src.UTF16(le) {
			fail("line_column", where)
		}
		rep.Checks["utf16_line_column"]++
		// target invariants
		if s.TargetText == "" || strings.ContainsAny(s.TargetText, "\r\n") || strings.ContainsRune(s.TargetText, ' ') {
			fail("target_newline_or_empty", where)
		}
		if te > le {
			fail("target_past_line_end", where)
		}
		if strings.TrimRight(s.TargetText, " \t\v\f") != s.TargetText {
			fail("target_trailing_whitespace", where)
		}
		if strings.Trim(string(b[te:le]), " \t\v\f") != "" {
			fail("non_whitespace_after_target", where)
		}
		rep.Checks["target_invariants"]++
		// contexts
		lcs, rce := s.LeftContextStartByteOffset, s.RightContextEndByteOffset
		if lcs < src.BOMLen || lcs > c || rce < te || rce > len(b) || string(b[lcs:c]) != s.LeftContext ||
			string(b[te:rce]) != s.RightContext || src.UTF16(lcs) != s.LeftContextStartUTF16Offset ||
			src.UTF16(rce) != s.RightContextEndUTF16Offset || s.LeftContextTruncated != (lcs > src.BOMLen) ||
			s.RightContextTruncated != (rce < len(b)) {
			fail("context_window", where)
		}
		if !strings.HasPrefix(s.RightContext, string(b[te:min(src.LineBreakEnd[l], rce)])) {
			fail("right_context_line", where)
		}
		rep.Checks["context"]++
		// deterministic id
		if id := StableID(s.RepositoryID, s.RelativePath, s.SourceSha256, strconv.Itoa(s.CaretUTF16Offset),
			strconv.Itoa(s.TargetEndUTF16Offset)); id != s.SampleID {
			fail("sample_id", where)
		}
		rep.Checks["sample_id"]++
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := validateSemantic(dataset, ids, rep, fail); err != nil {
		return nil, err
	}
	if err := validateCorpus(dataset, repo, rep, fail); err != nil {
		return nil, err
	}
	rep.OK = len(rep.Failures) == 0
	return rep, nil
}

func validateSemantic(dataset string, ids map[string]bool, rep *ValidationReport, fail func(string, string)) error {
	rc, err := OpenJSONL(filepath.Join(dataset, "semantic.jsonl"))
	if err != nil {
		return nil // no sidecar
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 1<<20), 1<<28)
	for sc.Scan() {
		rep.Semantic++
		var r struct {
			SampleID         string `json:"sample_id"`
			VisibilityPolicy string `json:"visibility_policy"`
			Status           string `json:"status"`
			Leakage          *struct {
				Violations []string `json:"violations"`
			} `json:"leakage"`
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			fail("semantic_json", err.Error())
			continue
		}
		if !ids[r.SampleID] {
			fail("semantic_orphan", r.SampleID)
		}
		switch r.Status {
		case "resolved", "partially_resolved", "syntax_fallback", "failed":
		default:
			fail("semantic_status", r.Status)
		}
		if r.Leakage != nil && len(r.Leakage.Violations) > 0 {
			fail("semantic_leakage", fmt.Sprintf("%s/%s: %v", r.SampleID, r.VisibilityPolicy, r.Leakage.Violations))
		}
		rep.Checks["semantic_leakage_audit"]++
	}
	return sc.Err()
}

// validateCorpus checks every corpus record against the repository bytes: sha256 of the original bytes and exact
// content (BOM + content == file bytes).
func validateCorpus(dataset, repo string, rep *ValidationReport, fail func(string, string)) error {
	rc, err := OpenJSONL(filepath.Join(dataset, "corpus.jsonl"))
	if err != nil {
		return nil // no corpus
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 1<<20), 1<<28)
	for sc.Scan() {
		var c CorpusFile
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			fail("corpus_json", err.Error())
			continue
		}
		b, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(c.RelativePath)))
		if err != nil {
			fail("corpus_source_missing", c.RelativePath)
			continue
		}
		bom := ""
		if c.HasBOM {
			bom = "\xEF\xBB\xBF"
		}
		if Sha256Hex(b) != c.Sha256 || bom+c.Content != string(b) || c.Bytes != len(b) || c.Language != "go" {
			fail("corpus_content", c.RelativePath)
		}
		rep.Checks["corpus"]++
	}
	return sc.Err()
}
