# Extraction rules (Go)

## File discovery

Paths are enumerated (symlinks not followed, `.git` pruned), sorted ordinally, and filtered in this order; every
decision is reason-coded in `discovery.jsonl`:

1. `discovery.include` (default `**/*.go`) — non-matching paths are not recorded.
2. `excluded_path`, `go_ignored_dir` (a path segment starts with `_` or `.`, which the go tool ignores),
   `testdata_path` (`testdata/**`), `vendored_path` (`vendor/**`, `third_party/**`, …), `too_large`,
   `generated_path` (`*.pb.go`, `*.pb.gw.go`, `*_gen.go`, `*.gen.go`, `zz_generated*.go`, `bindata.go`, …).
3. Decoding: `utf16_encoding`, `invalid_utf8`, `binary_nul`, `lone_cr`; `empty`.
4. `invalid_package_clause` (not parseable as Go — templates, `{{.X}}` files), then
   `generated_marker`: `go/ast.IsGenerated` (`// Code generated ... DO NOT EDIT.`) plus loose regex markers in the
   first 4000 bytes — tool-generated code is recognized by content, not only by name.
5. Build constraints evaluated with a fixed context (`//go:build`, legacy `// +build`, and GOOS/GOARCH filename
   suffixes via `go/build.MatchFile`, which only reads the header and never runs `go`). cgo files count as
   non-matching when cgo is disabled. `build_excluded` → kept (tag `build_excluded`) or skipped per
   `discovery.build_excluded_policy`.
6. `long_lines` (minified-like), `license_not_allowed` (repo license unknown/not allowlisted and
   `license.allow_unknown=false` / conflicts with a declared id), `secret_detected` (file-level patterns),
   `exact_duplicate` (sha256 already seen; `duplicate_of` names the first path).

Tests are **not** excluded; `is_test` (via `test_patterns`, default `**/*_test.go`) lets reports measure their
share separately. License is detected from the root LICENSE/COPYING text (MIT/Apache/BSD/ISC/… signatures); a
declared id that conflicts with the detected one blocks the repo (`license_conflict`).

## Caret candidates (syntax only)

Generated from tokens (`go/scanner`, `ScanComments`, auto-inserted semicolons dropped) plus AST facts
(`go/parser` with `AllErrors`) on each non-blank physical line. One candidate per offset (highest-priority kind
wins). Kinds and strata are listed in `docs/PLAN.md` §5. `member_access` subkinds: `package` (base is an imported
package name), `value`, `type_assertion` (`.(`), `call_result`. `error_handling` covers `if err != nil` heads and
bodies, `err`-returns inside them, and `errors.*` / `fmt.Errorf` call sites. `log_message` emits `template_start`
(before the format string's quote) and `template_body` (inside it, the one caret allowed inside a string literal).

## Negative strata (counted, sampled into `exclusions.jsonl`, never silently dropped)

Line level: `comment`, `directive` (`//go:`/`//line`/`//export`/`//nolint`), `build_constraint`,
`inside_raw_string`, `inside_block_comment`, `too_long`. Caret level: `in_string`, `in_raw_string`,
`in_rune_literal`, `in_comment`, `caret_inside_token`, `raw_multiline_string`, `multiline_comment`,
`empty_target`, `target_too_long`, `secret_detected`.

## Sampling

For each eligible candidate `u = uniform(seed ␟ "caret" ␟ source_sha256 ␟ offset)` in [0,1) (top 53 bits of
SHA-256, identical to the C# `Hashing.Uniform`). Accepted if `u < weight[kind]` (× `trivial_target_keep_probability`
for trivial targets). Then per line keep the `max_samples_per_line` lowest `u/weight`, then per file the
`max_samples_per_file` lowest; output in source order. Selection depends only on `(seed, file content, offset,
config)` — not on worker count or file order. Dataset-level: later samples whose (trimmed line, caret column within
the trimmed line) already occurred are dropped (`samples.dropped_duplicate`), removing boilerplate such as repeated
`import` lines. Records (contexts, flags, tags, offsets) are materialized only for survivors.

## Semantic snapshot

* `editor_snapshot`: `bytes[:caret] + bytes[target_end:]` — target suffix removed, the rest of the file stays.
* `strict_prefix`: `bytes[:caret]` + one closer per bracket left open by the prefix (computed from prefix tokens
  only, recorded in `synthetic_suffix`), so the caret sits inside its function instead of on EOF.

Facts are produced by type-checking the package with the snapshot file substituted (`analysis_engine`:
`package_typecheck`). Rules that keep facts honest:

* Locals/parameters declared at or after the caret, or in the declarator being typed, are excluded (scope walk uses
  `pos < caret`; re-checked by the audit).
* `editor_snapshot` uses **decl_splice**: when the parser's error recovery inside the edited declaration would drop
  the following top-level declarations, they are taken from the parsed **original** file (drop-only; the original is
  never used to add a fact that is not byte-identical in the snapshot). `strict_prefix` naturally drops
  declarations after the caret.
* A caret right after `.` gets a placeholder `_` (**phantom_selector**) so the selector base keeps its type.
* Imported-package members are type-checked from separately-compiled packages that never saw the snapshot, so their
  overlap with the target is coverage, not leakage.
* Leakage audit per record; `validate` fails on any violation (see `docs/PLAN.md` §7).

## Semantic engines (`semantic.engine`: `auto` | `snapshot`)

`auto` picks per record (`analysis_engine` in each record):

| Engine | When | How |
|---|---|---|
| `original_scope` | editor_snapshot, caret inside a function body | the ORIGINAL package (one build variant) is type-checked once and cached; facts are read at the caret position |
| `snapshot_typecheck` | everything else (package-level carets, all strict_prefix records); `--semantic-engine snapshot` forces it everywhere (reference) | the target-free snapshot (+ decl_splice / phantom_selector) is type-checked with every other function body stripped |

Why `original_scope` is leakage-safe (and the guards that make it so):

* go/types scopes start **after** the declaring statement / signature, so a local, parameter, label or local type
  declared on the edited line is never visible at the caret. Visibility is evaluated at `caret-1` (both engines), so
  the declarator being typed (`x := foo.▮`) is excluded even when a snapshot statement ends exactly at the caret.
* Selector bases, callees and assignment targets are expressions that end before the caret; their types do not
  depend on later text — except **generic functions and builtins**, whose recorded signature is instantiated from
  all arguments (possibly in the hidden target). For those, only the declared generic signature is emitted and no
  expected type (builtins: nothing).
* Package-level declarations cannot change when the edit is inside a function body.
* The leakage audit runs on every record with the snapshot's identifier set (package identifier counts minus the
  target's identifiers). A record that fails the audit is emitted **without facts**: `status: failed`,
  `reason: leak_audit:<names>`; the repository is never failed for it.

The reference comparison (`scripts/semantic_compare.py`) and its review are in `docs/BENCHMARKS.md`.

Imported packages (stdlib, other packages of the repository, `./vendor`) are type-checked once per run **without
function bodies** (declarations only). Import resolution is **offline** (`internal/semantic/loader.go`): standard
library from `$GOROOT/src` of the builder's toolchain (`GOROOT=/opt/go` on the server), in-module packages resolved
through the root `go.mod` module path, and `./vendor`. External module imports stay unresolved →
`partially_resolved` (`external_imports_unresolved` / `unresolved_import`). No `go list`, no downloads, no code
execution.

## Bulk-mode thinning (repository-wide, applied after dataset-level dedup)

The orchestrator first runs a syntax pass to count samples, then sets per repository:

* `sampling.keep_fraction` / `test_keep_fraction`: keep a sample iff `uniform(seed, "keep", sample_id) < keep`
  (× `test_keep_fraction` for `_test.go` code) — a hash of the content-derived id, independent of path order and
  worker count (same rule as the C# pipeline);
* `sampling.max_samples_per_repo`: if still above the cap, keep the samples with the smallest
  `uniform(seed, "repo_cap", sample_id)` repository-wide (not the first N in path order), source order preserved.

Counters: `samples.dropped_thinning`, `samples.dropped_thinning_test`, `samples.dropped_repo_cap`.
