# Benchmark methodology & pilot results

Reproduce: `python3 -I scripts/benchmark.py --repo data/repos/ttlcache --repo data/repos/httpmock --workers 8
--subset 0.15`. Each experiment runs as an isolated `goflc` child process (cold run discarded, warm run reported);
every number is parsed from a real `run-manifest.json` — nothing is hand-entered. Machine facts (CPU, RAM, kernel,
Go version) are recorded in each manifest's `environment` block.

## Experiments

| Experiment | Description | Purpose |
|---|---|---|
| E0 | discovery + filtering counts | reliable source counts and input-byte volume (in each run's `summary.json`) |
| E1 seq | syntax-only, `--workers 1` | correct canonical FLC samples + peak throughput baseline |
| E1 par | syntax-only, `--workers N` | sequential vs bounded-parallel (no unbounded queues) |
| E2 | semantic enrichment of a fixed subset (`semantic.subset_fraction`) | extra value and cost of type/scope info |
| E3 | full semantic enrichment | realistic scaling, fallbacks, memory |
| E4 | `editor_snapshot` vs `strict_prefix` on the same samples | leakage risk / information loss |
| E5 | re-run same revision/config/seed | determinism (checked by the test suite: byte-identical outputs) |

Separate timings are recorded per stage (discovery, syntax, semantic, write) in `timings.stages`, so semantic cost
is never mislabeled as syntax throughput. Peak RSS is read from `/proc/self/status` (`VmHWM`).

## Pilot results (2 repos; see `artifacts/benchmarks/benchmark.{json,md}` for the committed run)

Environment of the committed run is in `benchmark.json` (Linux x86_64, Go 1.27.2). Representative warm numbers:

| repo | files (test) | samples | E1 seq samples/s | E1 par (8) samples/s | E1 peak RSS | E3 semantic samples/s | E3 peak RSS |
|---|---|---|---|---|---|---|---|
| ttlcache | 18 (5) | 1676 | ~27k | ~35k | ~21 MB | ~154 | ~1.4 GB |
| httpmock | 24 (13) | 2151 | ~28k | ~41k | ~22 MB | ~165 | ~1.1 GB |

Observations:

* **Syntax-only** is fast and light; parallelism gives a sub-linear speedup on these tiny repos (startup and the
  single large file dominate) — the win grows with repo size. Both seq and par outputs are byte-identical and pass
  `validate` (full reconstruction in bytes and UTF-16).
* **Semantic** is ~150–170 samples/s and ~1–1.4 GB RSS, dominated by `go/types` checking each package against the
  standard library parsed from source. This is the top bottleneck; §Scale plan describes the per-package
  compilation cache / incremental-binding fix.
* **E4 leakage:** `leakage_violations = 0` across every record under both policies (enforced by `validate`).
  `editor_snapshot` resolves slightly more carets than `strict_prefix` (later declarations visible); target
  identifier overlap is similar and is coverage, not leakage. Many records are `partially_resolved` with
  `external_imports_unresolved` because external modules are not downloaded — honest, reason-coded behavior.

Empty/unmeasured cells are never reported as a completed benchmark; a blocked mode is recorded as `skipped` with a
reason in `benchmark.json`.
