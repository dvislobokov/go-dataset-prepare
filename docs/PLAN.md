# Go Full-Line Completion dataset pipeline — architecture & plan

Status: **stage 2** — stage 1 (discovery + syntax extraction + semantic prototype + validation + benchmark) plus a
fast cached semantic engine (5–7× single-thread, 10–12× with 8 workers on the bulk config), `goflc render`
(`flc-prompt/v2`), bulk-mode options and the bulk orchestrator `scripts/goflc_run.py` (dry-run locally). Modeled on the C# / Roslyn pipeline in
`/data/dataset-test`; this document records what was carried over, what is Go-specific, and what comes next.

## 1. Mission and scope

Build a reproducible, observable, semantic-aware dataset pipeline for training a small local decoder-only **Go**
full-line completion (FLC) model for the same IntelliJ plugin family as the C# work. Same two products, same
schema field names where the concept is shared, same quality bar: correctness, data quality, reproducibility,
observability, measured throughput — not model training or IDE integration.

The future scale target (≈34k repositories from a user-supplied manifest) is **designed for, not executed**
(see §10, `docs/SCALE_PLAN.md`). This stage processes only local fixtures and a 2-repo pilot.

## 2. Language choice: implement the extractor in Go

The extractor is written in **Go**, using the standard toolchain packages:

- `go/scanner`, `go/token` — exact token spans and line tables.
- `go/parser`, `go/ast` — syntax tree for caret classification and AST facts.
- `go/build`, `go/build/constraint` — build-tag / GOOS-GOARCH file selection (`MatchFile`) with **no** `go` command.
- `go/types` + `go/importer` — semantic analysis.

Rationale (parity with "Roslyn is the primary implementation language" for C#): the authoritative Go front end is
the standard library itself. Using it guarantees the tokenizer, build-constraint logic and type checker match what
`gopls`/`go vet` see, avoids re-implementing Go lexing in another language, and keeps offsets in the units Go tools
use. `golang.org/x/tools/go/packages` is **deliberately not used**: `packages.Load` shells out to `go list`, which
triggers module graph resolution and network downloads — incompatible with the "never build or download" safety
tier. Instead we drive `go/types` directly with a custom offline `Importer` (§7). Only standard-library packages
(`std`), in-module packages (resolved through `go.mod`), and `./vendor` are importable; external modules stay
unresolved and are reason-coded. The module has **no third-party dependencies** (`go.mod` requires nothing),
so it builds and tests with `GOPROXY=off`.

## 3. Coordinates and newlines (the critical rules)

- **Byte offsets are authoritative.** Go tooling addresses source by byte offset (`go/token.File.Offset`,
  `gopls` internals, LSP `didChange` on bytes). Every sample stores `*_byte_offset` and the IDE-facing
  `*_utf16_offset` (UTF-16 code units into the decoded text with the BOM removed) so the IntelliJ plugin, which
  addresses documents in UTF-16, can map positions. The schema documents which is which; `validate` round-trips
  both and fails on any disagreement, including inside surrogate pairs, tabs, and with a BOM.
- Hashes (`source_sha256`) are over the **original bytes** (BOM included). Reconstruction is proven on those bytes.
- Lines/columns are **zero-based**; columns are given in both byte and UTF-16 units.
- Newlines: **LF and CRLF only.** The Go scanner treats a lone `\r` as whitespace, which would make "physical line"
  ambiguous, so files containing a lone CR are rejected at discovery (`lone_cr`). `end_of_line ∈ {LF, CRLF, EOF}`.
- `target_text` = rest of the physical line after the caret **excluding trailing whitespace** (which begins
  `right_context`); never contains a line break; reconstruction invariant
  `bytes == bytes[:caret] + target_text + bytes[target_end:]`. Indentation before a `line_start` caret is treated
  as already typed (editor auto-indent). After keywords/commas/operators the caret is placed after the typed
  whitespace separator (gofmt alignment counts as typed). These conventions mirror the C# "Implementation
  decisions" and are in `docs/DATASET_SPEC.md`.

## 4. Data contract (two products)

Reuses the C# field names; Go additions are byte-offset twins plus `language:"go"`, `package_name`,
`build_constraint`, `build_match`. Schemas: `schemas/*.go.schema.json` (+ shared `run-manifest.v1.schema.json`).

- **A. Source corpus** (`corpus.jsonl`, `corpus-file/v1`): one record per accepted file, exact bytes preserved,
  `content_normalized:false`, newline style + BOM recorded.
- **B. FLC samples** (`samples.jsonl`, `flc-sample/v1`): one caret sample; offsets, contexts, tags, quality flags,
  split, deterministic `sample_id`.
- **Semantic sidecar** (`semantic.jsonl`, `flc-semantic/v1`): keyed by `(sample_id, visibility_policy)`.
- Plus `discovery.jsonl` (every candidate path + reason), `exclusions.jsonl` (audit examples), `summary.json`
  (deterministic counters), `run-manifest.json` (provenance/timings/resources/checksums), `validation.json`.

`sample_id = sha256(repository_id, relative_path, source_sha256, caret_utf16, target_end_utf16)[:32]` — identical
to C#, content-derived, stable across workers/order/revisions.

## 5. Go caret strata

Generated from tokens + AST on each non-blank physical line; one candidate per offset (highest-priority kind wins),
hash-sampled by configurable per-kind weight, then capped per line and per file (see `docs/EXTRACTION_RULES.md`).

| Kind | Positions |
|---|---|
| `line_start` | after indentation; subkind = statement/decl kind (`short_var_decl`, `return`, `if`, `func_decl`, `field`, `case`, `send`, …) |
| `identifier_partial` | 1..N runes into an identifier + camelCase / `_` humps; never splits a rune |
| `member_access` | after `.` (selector / `pkg.` / method / field / type assertion) |
| `argument_list` | after `(`/`[` of a call or param list, after `,` (+ space) |
| `composite_literal` | after `{`, after `,`, after `key:` in struct/map/slice literals |
| `after_keyword` | after `return go defer range case var const type func import chan map` (+ space) |
| `after_operator` | after `= := += == && || <- …` (+ space) |
| `control_flow` | after `if for switch select` head |
| `func_literal` | at `func` of a closure (subkind: `go`, `defer`, `call_argument`, `assignment`, …) |
| `error_handling` | `if err != nil`, the body of such an `if`, `err`-returns, `fmt.Errorf`/`errors.{New,Wrap,Is,As}` call sites |
| `log_message` | format/message string of `fmt.*`, `log.*`, `slog.*`, `logrus`/`zap`/`zerolog`-style calls |
| `token_boundary` | end of any token (low weight) |

**Negative strata are counted, never silently dropped** (`exclusions.jsonl` + `summary.json`): comments,
doc comments, `//go:` directives, build-constraint lines, inside raw/interpreted strings and rune literals,
block-comment and raw-string interiors, too-long lines, multiline-string targets, secret lines, carets that split a
rune. Build-tag-excluded files (`build_match=false`) are kept (tagged `build_excluded`) or skipped per config.

Tags: `error_handling`, `log_call`, `closure`, `goroutine`, `defer`, `select`, `switch`, `channel`,
`composite_literal`, `struct_type`, `interface_type`, `generics`, `test_code`, `build_excluded`, `cgo`.

## 6. Go semantic facts (target-free)

Facts are computed on a snapshot with the hidden target removed, under two visibility policies:

- `editor_snapshot`: `bytes[:caret] + bytes[target_end:]` — the rest of the file stays (cross-file / later-decl
  awareness; requires leakage auditing).
- `strict_prefix`: `bytes[:caret]` + one closer per bracket left open by the prefix (`synthetic_suffix`), so the
  caret sits inside its function instead of on EOF; a leakage-control ablation.

Facts (emit only what `go/types` resolves): `locals`, `parameters` (incl. named results, closure params, receiver),
`receiver_members` (Go's "this": fields incl. promoted + method set), `package_members` (package-level decls in
scope), `members` after `.` (accessible fields/methods of the receiver type, or exported members of an imported
package; **all** applicable candidates, never the one the target uses), `receiver_type`/`receiver_kind`,
`call_signature` + `argument_index` + `expected_type` (`expected_type_source ∈ argument|return|assignment`), and
`context_types` — contracts of nearby **repository** named types reachable from in-scope locals/params/receiver
fields (the TYPE-block idea from the C# `flc-prompt/v2`), leakage-safe because candidates come only from facts that
exist before the caret. `imports` records which import paths resolved offline.

Two parse-robustness devices keep facts on broken snapshots, both using only visible editor text and recorded in
`snapshot_repairs`: **phantom_selector** (a caret right after `.` gets a placeholder `_`, like gopls, so `x.` stays
a selector) and **decl_splice** (editor_snapshot only: the parser's error recovery inside the edited declaration can
swallow the following top-level declarations, so every declaration except the edited one is taken from the parsed
original file — drop-only use of the original, analogous to the C# "recovery artifacts" rule).

Engines (stage 2): `original_scope` reads facts from the cached type-checked original package for editor_snapshot
carets inside function bodies (scopes start after the declaring statement, so nothing in the hidden target is
visible; generic/builtin calls report only declared signatures); `snapshot_typecheck` checks the snapshot with all
other function bodies stripped for the rest and for strict_prefix. See `docs/EXTRACTION_RULES.md`.

Status: `resolved | partially_resolved (reason) | syntax_fallback (reason) | failed (reason)`; a record failing the
leakage audit becomes `failed` / `leak_audit:<names>` without facts. External modules are
not downloaded, so package-heavy code degrades to `partially_resolved` with `external_imports_unresolved` /
`unresolved_import` rather than inventing facts.

## 7. Leakage rules & audit

Every semantic record carries a `leakage` block audited per record; `validate` fails on any `violations`:

- A local/parameter whose declaration is **at or after the caret** in the snapshot never appears
  (`local_declared_after_caret` / in strict_prefix `declared_in_synthetic_suffix`). Enforced by the scope walk
  (only objects with `pos < caret`) and re-checked in the audit.
- A fact declared **in the analyzed package** whose name occurs in the hidden target but **nowhere in the snapshot
  package sources** is a violation (`name_only_in_target`): it could only have come from the target.
- Names that legitimately exist independently (members of imported packages type-checked without ever seeing the
  snapshot; symbols used elsewhere in the file) may overlap the target — recorded as `mentioned` (coverage), not a
  violation, exactly as the C# policy specifies.

`members`/`call_signature` always list every accessible candidate (capped), never the single answer.

## 8. Safety tiers

Input code is untrusted and is **never built or executed**: no `go build`, `go generate`, `go test`, cgo
compilation, or module download at any point. See `docs/SECURITY.md`.

| Mode | Executes input code? | Network | Notes |
|---|---|---|---|
| `discover`, `extract --semantic none`, `validate` | No | No | Pure file reads + `go/parser`. Safe default. |
| `extract --semantic best_effort` | No | No | `go/types` with the **offline** importer: stdlib (from the builder's own `$GOROOT`), in-module and `./vendor` packages only; external imports resolve to errors (reason-coded). No `go list`, no MSBuild-equivalent, no downloads. |
| `--trusted-project-evaluation` (future) | would run `go list`/module download | Yes | **Not implemented**; reserved for a sandboxed trusted tier (see SCALE_PLAN). Refused today. |

Generated code is dropped by content (`// Code generated ... DO NOT EDIT.` via `go/ast.IsGenerated`, plus loose
markers) and by path (`*.pb.go`, `*_gen.go`, `zz_generated*`, vendor/, testdata/, `_`/`.`-prefixed dirs). License is
re-detected from the repo LICENSE text and gated per file; secrets filtered at file level (high-confidence formats)
and line level (literal-only credential assignments, UPPER_SNAKE env names excluded).

## 9. Pipeline & determinism

Streaming discovery → bounded worker pool (default sequential; `--workers N` with a reorder buffer bounded at
`4×workers` files in flight, no unbounded queues) → ordered writer → optional semantic pass (one `go/types` importer
per run, packages parsed once, per-package snapshots). Atomic output dir (`<out>.tmp-pid` → rename; refuses
overwrite without `--overwrite`). Determinism: content-derived ids, hash-based sampling on `(seed, source_sha, offset)`,
source-order output, map-free counter merge — byte-identical `samples/semantic/summary` across worker counts
(tested). A sequential baseline exists before any parallel run (benchmark E1 seq vs E1 par).

## 10. Scale readiness (designed, not executed)

`docs/SCALE_PLAN.md`: manifest reader interface (txt/csv/jsonl), per-repo job states, repository-group splits
(fork lineage + content-overlap) assigned **before** caret expansion, per-file/project/repo caps, sparse shallow
clone of pinned SHAs, atomic shards + checksums + resumable state, offline semantic tier for all repos with a
sandboxed trusted tier for eligible ones. No bulk downloader is built in this phase.

## 11. Risks / open decisions

- **Semantic cost & memory** (addressed in stage 2): cached original-package engine + body-less imports + stripped
  snapshot checks; remaining costs are the per-process stdlib load and the snapshot fallback for package-level
  carets (`docs/BENCHMARKS.md`).
- **External imports unresolved offline** → many `partially_resolved`. Honest and reason-coded; a sandboxed trusted
  tier with a module mirror would raise `resolved` rates (future).
- **Build-constraint matrix.** Only one GOOS/GOARCH is analyzed per run; `build_excluded` files get syntax samples
  but no semantics. A multi-context pass is possible later.
- **Generics / type inference** edge cases in `expected_type` are best-effort; unresolved cases emit nothing rather
  than guessing.

## 12. Milestones

1. **(done)** Discovery + filters, caret strata, JSONL + schemas, exact-reconstruction validator, unit tests,
   candidate search + selection, Go toolchain install, syntax benchmark, 2-repo pilot.
2. **(done, prototype)** Semantic facts with go/types, two visibility policies, leakage audit + hard tests.
3. **(done)** Semantic performance (cached original scope, stripped bodies, parallel chunks); `goflc render`
   (`flc-prompt/v2`, `<|eol|>` stop token); bulk thinning options; leak-audit quarantine.
4. **(done, dry-run)** Bulk orchestrator `scripts/goflc_run.py` → HF `dvislobokov/go-ml-complation`
   (`docs/DEPLOY.md`); the server run is started by the user.
5. Next: long-lived worker sharing the stdlib across repositories; fast engine for safe package-level carets;
   TYPE-budget study; MinHash near-duplicate grouping across repositories; optional `package_members` line in
   the prompt.
