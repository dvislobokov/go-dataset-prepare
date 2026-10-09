# Benchmark methodology & pilot results

Reproduce: `python3 -I scripts/benchmark.py --repo data/repos/ttlcache --repo data/repos/httpmock --workers 8
--subset 0.15`. Each experiment runs as an isolated `goflc` child process (cold run discarded, warm run reported);
every number is parsed from a real `run-manifest.json` — nothing is hand-entered. Machine facts (CPU, RAM, kernel,
Go version) are recorded in each manifest's `environment` block (this machine: 16 vCPU, 27 GB RAM, Linux 6.14, Go 1.27.2).

## Experiments

| Experiment | Description | Purpose |
|---|---|---|
| E0 | discovery + filtering counts | reliable source counts and input-byte volume (in each run's `summary.json`) |
| E1 seq / par | syntax-only, `--workers 1` / `N` | correct canonical FLC samples + throughput baseline; bounded parallelism |
| E2 | semantic enrichment of a fixed subset (`semantic.subset_fraction`) | extra value and cost of type/scope info |
| E3 | full semantic enrichment, both visibility policies | realistic scaling, fallbacks, memory |
| E3e seq / par | full semantic, `editor_snapshot` only | **the bulk-run configuration** (`configs/go.bulk.json`) |
| E4 | `editor_snapshot` vs `strict_prefix` on the same samples | leakage risk / information loss |
| E5 | re-run same revision/config/seed | determinism (test suite + byte-identical w=1 vs w=8 check) |

Stage timings are separated (discovery, syntax, semantic, write in `timings.stages`), so semantic cost is never
mislabeled as syntax throughput. Peak RSS is `VmHWM` from `/proc/self/status`.

## Stage 2: semantic performance (before → after)

"Before" is the stage-1 binary (commit `738e5d7`) rebuilt from a temporary worktree and run on the same machine,
same repos and same configs. Samples/s are end-to-end `extract` throughput (syntax + semantic).

| repo | config | stage 1 | stage 2, 1 worker | stage 2, 8 workers |
|---|---|---|---|---|
| ttlcache (1676 samples) | both policies | 153 /s, 10.9 s, 1.39 GB | 534 /s, 3.1 s, 0.59 GB | 1674 /s, 1.0 s, 0.96 GB |
| ttlcache | editor_snapshot (bulk) | 257 /s, 6.5 s, 1.28 GB | 1429 /s, 1.2 s, 0.43 GB | 2668 /s, 0.6 s, 0.53 GB |
| httpmock (2151 samples) | both policies | 164 /s, 13.1 s, 1.06 GB | 917 /s, 2.3 s, 0.46 GB | 2383 /s, 0.9 s, 0.58 GB |
| httpmock | editor_snapshot (bulk) | 291 /s, 7.4 s, 1.02 GB | 1992 /s, 1.1 s, 0.33 GB | 3560 /s, 0.6 s, 0.33 GB |

Speed-up on the bulk configuration: **5.6× / 6.8× single-threaded, 10.4× / 12.2× with 8 workers** (stage 1 ran the
semantic pass sequentially regardless of `--workers`); against the stage-1 headline (~160 samples/s) the bulk config
is 9–12× single-threaded. On these tiny repos ~0.3 s of each run is the one-time standard-library load, so the
single-thread ratio grows with repository size. Memory dropped 2–4×.

What changed (all in `internal/semantic`):

1. **Imported packages are checked without function bodies** (stdlib and in-repo dependencies; only declarations
   matter for importers — what export data would contain). Thread-safe loader cache per repository run.
2. **original_scope engine** (editor_snapshot, caret inside a function body — ~76–84% of records): the original
   package is type-checked once per build variant and cached; facts are read at the caret position. This is the Go
   analogue of binding against the cached Roslyn compilation. Safety arguments and guards are in
   `docs/EXTRACTION_RULES.md` (semantic engines): go/types scopes start after the declaring statement, so nothing
   declared in the hidden target is visible at the caret; generic and builtin calls never report the instantiation
   recorded on the full file (it may be inferred from arguments in the target) — only the declared generic signature.
3. **snapshot_typecheck engine** (fallback: package-level carets, and all strict_prefix records): the target-free
   snapshot is type-checked with **all other function bodies stripped**, so only the edited function is re-bound.
4. Chunked parallelism inside packages with a shared per-package cache; records written in deterministic
   (package, chunk) order — outputs are byte-identical for 1 and 8 workers.

### Fact agreement (original_scope vs reference)

`scripts/semantic_compare.py` compares per-record facts of `--semantic-engine auto` against the reference
`--semantic-engine snapshot` on the editor_snapshot policy: **96.8% (ttlcache) and 98.6% (httpmock) of records are
identical**. The differing records were reviewed; in every inspected case the fast engine is equal or more correct:

* the snapshot parse loses facts after error recovery (closure parameters, locals, method sets of fields) — the
  original scope keeps them (they are declared before the caret and visible in the editor);
* the snapshot turns locals into package-level declarations (recovery artifacts) — not present in the original;
* generic calls: the fast engine emits only the declared generic signature and no expected type (more conservative).

The review also found and fixed real problems: (a) on the full file go/types records **builtin** signatures
instantiated from the actual arguments (`len(▮mfs)` → `[]MatcherFunc`, taken from the hidden target) — builtins now
get no signature/expected type on the fast engine (test `TestOriginalEngineGenericAndBuiltinCallsDoNotLeakInstantiation`,
mutation-checked); (b) the snapshot engine exposed the declarator being typed (`client1 := &http.Cl▮ient{}` → local
`client1`) because the snapshot statement ends exactly at the caret — visibility is now evaluated at `caret-1` in
both engines; (c) a stage-1 bug: the per-variant parse cache omitted the first analyzed file from later checks of
the same package.

Records whose facts fail the leakage audit are emitted without facts (`status: failed`,
`reason: leak_audit:<names>`), never failing the repository; on the pilot this happened 0 times.

## Pilot results (committed run: `artifacts/benchmarks/benchmark.{json,md}`)

| repo | files (test) | samples | E1 seq /s | E1 par (8) /s | E3e seq /s | E3e par /s | E3 par /s, RSS | validate | leakage violations |
|---|---|---|---|---|---|---|---|---|---|
| ttlcache @230d2c7 | 18 (5) | 1676 | ~21k | ~25k | ~1.4k | ~2.7k | ~1.7k, ~0.96 GB | ok | 0 |
| httpmock @ae9b916 | 24 (13) | 2151 | ~21k | ~27k | ~2.0k | ~3.6k | ~2.4k, ~0.58 GB | ok | 0 |

Observations:

* **Syntax-only** remains far faster than semantic; both scale sub-linearly on these tiny repos.
* **Semantic** is now dominated by (1) the one-time stdlib load per process (~0.3 s), (2) the snapshot fallback for
  package-level carets (~20% of records), (3) GC. Next steps: share a pre-loaded stdlib across repositories in a
  long-lived worker; extend the fast engine to safe package-level positions.
* **E4:** `leakage_violations = 0` under both policies (enforced by `validate`). Many records are
  `partially_resolved` (`external_imports_unresolved`) because external modules are not downloaded — honest,
  reason-coded behavior of the offline tier.

## Orchestrator dry run (stage 2)

`scripts/goflc_run.py --no-upload --keep-local` on the 3 smallest selected repositories (sparse shallow clones in a
temporary work dir, each deleted after its job):

| repository | revision | Go files | samples | outcome | job time |
|---|---|---|---|---|---|
| github.com/icattlecoder/godaemon | ae28f9f | 2 | 47 | complete | 2.0 s |
| github.com/bingcicle/friendly-potato | e716773 | 1 | 108 | complete | 1.9 s |
| github.com/xudongzheng/gitstreak | 8f2aa2e | 1 | 183 | complete | 2.0 s |

One batch: `samples/train` 338 rows (43 columns), `semantic/train` 338 (31), `prompts/train` 338 (9), `repos` 3 (14);
semantic: 320 resolved, 13 partially_resolved, 5 syntax_fallback (`no_package_clause` — caret on the package line);
278/338 prompts carry a `<|sem|>` block; every repository passed `validate`. A `--corpus-only` pass produced
`corpus/train` 4 rows. A `kill -9` during the semantic stage was salvaged into all samples + the completed semantic
records, which pass `validate` (partial outcome path).
