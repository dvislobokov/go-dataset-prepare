# Dataset specification (v1, Go profile)

All outputs of one run live in one directory, written atomically (`<out>.tmp-<pid>` → rename).

| File | Schema | One record per |
|---|---|---|
| `discovery.jsonl` | `discovery-file/v1` (see `DiscoveryRecord` in `internal/flc/discovery.go`) | every candidate `.go` path, accepted or skipped with `skip_reason` |
| `corpus.jsonl` | [`corpus-file.v1.go`](../schemas/corpus-file.v1.go.schema.json) | accepted source file (product A); written only with `--corpus` |
| `samples.jsonl` | [`flc-sample.v1.go`](../schemas/flc-sample.v1.go.schema.json) | caret sample (product B) |
| `semantic.jsonl` | [`flc-semantic.v1.go`](../schemas/flc-semantic.v1.go.schema.json) | (sample, visibility policy); semantic modes only |
| `exclusions.jsonl` | `flc-exclusion/v1` | audit examples of excluded carets/lines (capped per reason; full counts in `summary.json`) |
| `summary.json` | deterministic counters (byte-identical across reruns) | the run |
| `run-manifest.json` | [`run-manifest.v1`](../schemas/run-manifest.v1.schema.json) | provenance, config, environment, timings, resources, output checksums |
| `validation.json` | written by `validate` | check counts and failures |

`--gzip` writes `*.jsonl.gz`; readers accept both.

## Coordinates and text

* **Byte offsets are authoritative** (Go tooling uses them). `*_byte_offset` index the original bytes (BOM included).
* `*_utf16_offset` are UTF-16 code units into the decoded text with any UTF-8 BOM removed (for the IntelliJ/LSP side).
* `source_sha256` hashes the exact original bytes = `(has_bom ? EF BB BF : "") + UTF8(content)`.
* Files that are not valid UTF-8, are UTF-16, contain NUL, or contain a lone CR are skipped with a reason.
* Lines/columns are zero-based; columns are stored in both byte and UTF-16 units. Line breaks: LF or CRLF only.

## Sample invariants (checked by `validate` on every sample against the repository bytes)

```
b = original bytes of the file
b[caret_byte .. target_end_byte] == target_text
b == b[:caret_byte] + target_text + b[target_end_byte:]
left_context  == b[left_context_start_byte .. caret_byte]      (<= context.left_chars UTF-16 units; never splits a rune)
right_context == b[target_end_byte .. right_context_end_byte]   (trailing whitespace, then the line break if present)
target_text has no line break, is non-empty, does not end with whitespace
b[target_end_byte .. line_end_byte] is whitespace only
utf16(x_byte) agrees with x_utf16 for every offset, and round-trips back to the same byte offset
```

**Whitespace convention.** Indentation before a `line_start` caret is already typed (editor auto-indent). After
keywords/commas/operators the caret is placed after the typed whitespace separator — including gofmt alignment runs,
so a target never begins with a space for those kinds. Trailing whitespace of a line is never part of the target
(it starts `right_context`; flagged `trailing_whitespace`).

**Sample id** = `sha256(repository_id ␟ relative_path ␟ source_sha256 ␟ caret_utf16 ␟ target_end_utf16)[:32]` —
content-derived; identical file content yields identical ids at any revision, worker count, or run order.

## Quality flags

`trivial_target` (only `{}()[];,`), `target_starts_with_whitespace`, `target_starts_with_closer`
(`)`/`]`/`}` first), `trailing_whitespace`, `target_has_comment`, `file_has_syntax_errors`, `non_ascii_target`.

## Splits

Repository-level `split` (pilot: `train`). At scale, splits are assigned to a repository **group** before caret
expansion (fork lineage + content overlap); `split_group` carries the group id. `validate` fails if one group
appears in two splits.

## Semantic sidecar

Keyed by `(sample_id, visibility_policy)`; status ∈ `resolved | partially_resolved | syntax_fallback | failed` with
a reason code. See `docs/EXTRACTION_RULES.md` §semantic and the schema. The sample's own
`semantic_status`/`semantic_reason` mirror the `editor_snapshot` result (or `not_attempted`).
