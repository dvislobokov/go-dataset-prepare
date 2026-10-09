package flc

// Sample is flc-sample/v1 (Go profile). Field names follow the C# pipeline; Go-specific additions are byte-offset
// twins of the UTF-16 fields, language, package_name and build information. See schemas/flc-sample.v1.go.schema.json.
type Sample struct {
	SchemaVersion               string   `json:"schema_version"`
	SampleID                    string   `json:"sample_id"`
	Language                    string   `json:"language"`
	RepositoryID                string   `json:"repository_id"`
	Revision                    *string  `json:"revision"`
	RelativePath                string   `json:"relative_path"`
	Project                     string   `json:"project"`
	PackageName                 string   `json:"package_name"`
	IsTest                      bool     `json:"is_test"`
	SourceSha256                string   `json:"source_sha256"`
	CaretUTF16Offset            int      `json:"caret_utf16_offset"`
	CaretByteOffset             int      `json:"caret_byte_offset"`
	CaretLine                   int      `json:"caret_line_zero_based"`
	CaretColumnUTF16            int      `json:"caret_column_utf16_zero_based"`
	CaretColumnByte             int      `json:"caret_column_byte_zero_based"`
	TargetEndUTF16Offset        int      `json:"target_end_utf16_offset"`
	TargetEndByteOffset         int      `json:"target_end_byte_offset"`
	LineStartUTF16Offset        int      `json:"line_start_utf16_offset"`
	LineStartByteOffset         int      `json:"line_start_byte_offset"`
	LineEndUTF16Offset          int      `json:"line_end_utf16_offset"`
	LineEndByteOffset           int      `json:"line_end_byte_offset"`
	CaretKind                   string   `json:"caret_kind"`
	CaretSubkind                *string  `json:"caret_subkind"`
	Tags                        []string `json:"tags"`
	LeftContextStartUTF16Offset int      `json:"left_context_start_utf16_offset"`
	LeftContextStartByteOffset  int      `json:"left_context_start_byte_offset"`
	LeftContextTruncated        bool     `json:"left_context_truncated"`
	LeftContext                 string   `json:"left_context"`
	TargetText                  string   `json:"target_text"`
	RightContext                string   `json:"right_context"`
	RightContextEndUTF16Offset  int      `json:"right_context_end_utf16_offset"`
	RightContextEndByteOffset   int      `json:"right_context_end_byte_offset"`
	RightContextTruncated       bool     `json:"right_context_truncated"`
	EndOfLine                   string   `json:"end_of_line"`
	Indentation                 string   `json:"indentation"`
	QualityFlags                []string `json:"quality_flags"`
	Split                       string   `json:"split"`
	SplitGroup                  string   `json:"split_group"`
	BuildConstraint             *string  `json:"build_constraint"`
	BuildMatch                  bool     `json:"build_match"`
	SemanticStatus              string   `json:"semantic_status"`
	SemanticReason              *string  `json:"semantic_reason"`
	ConfigVersion               string   `json:"config_version"`
	ConfigSha256                string   `json:"config_sha256"`
	Generator                   string   `json:"generator"`
}

// Exclusion is an audit example of an excluded caret or line (flc-exclusion/v1).
type Exclusion struct {
	SchemaVersion string `json:"schema_version"`
	RelativePath  string `json:"relative_path"`
	Line          int    `json:"line_zero_based"`
	ByteOffset    int    `json:"byte_offset"`
	Reason        string `json:"reason"`
	CaretKind     string `json:"caret_kind"`
	LineText      string `json:"line_text"`
}

// CorpusFile is corpus-file/v1 with language "go".
type CorpusFile struct {
	SchemaVersion     string  `json:"schema_version"`
	RepositoryID      string  `json:"repository_id"`
	Revision          *string `json:"revision"`
	RelativePath      string  `json:"relative_path"`
	Language          string  `json:"language"`
	Sha256            string  `json:"sha256"`
	Bytes             int     `json:"bytes"`
	License           *string `json:"license"`
	LicenseReason     *string `json:"license_reason"`
	Encoding          string  `json:"encoding"`
	HasBOM            bool    `json:"has_bom"`
	NewlineStyle      string  `json:"newline_style"`
	Project           string  `json:"project"`
	PackageName       string  `json:"package_name"`
	IsTest            bool    `json:"is_test"`
	Split             string  `json:"split"`
	Lines             int     `json:"lines"`
	BuildConstraint   *string `json:"build_constraint"`
	BuildMatch        bool    `json:"build_match"`
	Content           string  `json:"content"`
	ContentNormalized bool    `json:"content_normalized"`
}

const Generator = "goflc/0.1.0"
