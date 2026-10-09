# Scale plan: bulk Go run

Stage 2 adds the bulk orchestrator `scripts/goflc_run.py` (explicitly requested), adapted from the C# `flc_run.py`:
resumable SQLite state, GraphQL metadata prefetch (fork/archived/default branch), sparse shallow blobless clone with
the token passed via environment, syntax pass → repository-wide thinning → semantic pass under a timeout/RSS
watchdog, partial salvage / syntax fallback, `validate`, `render`, Parquet per repository in a subprocess,
cross-repository exact-file dedup, batched idempotent Hugging Face upload in a separate thread, smallest-first plus a
big lane, deletion of each checkout, a `--corpus-only` pass and a README with real examples. It has been dry-run
locally on 3 repositories (`--no-upload`); the bulk run itself is deployed and started by the user
(`docs/DEPLOY.md`). The design notes below remain the contract.

## Workflow

```
manifest (txt/csv/jsonl) ──validate──▶ job store (pending)
   └─ license/provenance gate ─▶ skipped(reason)
pending ─▶ cloning (pinned SHA, blobless/shallow, size quota, timeout) ─▶ ready
ready ─▶ processing: discover → syntax → [semantic: offline | trusted sandbox] → validate → write shard ─▶ complete
any ─▶ failed(error, attempts++) ─▶ pending (exponential backoff + jitter, max attempts)
```

* **Manifest reader** (interface): accepts `txt` (one URL/id per line), `csv`, `jsonl` with repository URL/id and
  optional revision, license/provenance, include/exclude, priority. Allowlisted scheme/host, canonical id
  `host/owner/name`, credentials rejected in URLs, path traversal and shell metacharacters rejected, revision
  validated, duplicates rejected. Do not assume entries are GitHub-hosted, public, buildable, or licensed.
* **Selection (done, metadata-only)**: `data/selection/selection_script.py` turns the GitHub search dump into
  `selected.jsonl` with `repository_id`, `revision:null`, `license`, `priority`, `category`, `group`,
  `default_branch`, `flags`. License allowlist, abuse/junk filters, owner cap, union-find dedup groups, Go-toolchain
  exclusions, giant-repo rejection. Reproducible (`python3 -I`), reason-coded, no network.
* **Fetch (later)**: `git clone --filter=blob:none --no-checkout` + `checkout <sha>`, per-host concurrency and rate
  limits, honor API rate-limit headers, repo-size/file-count quotas, credentials only via a scoped helper, never in
  URLs/logs. `scripts/pilot_clone.sh` is the safe shallow-clone used for the pilot.
* **Shards**: one output directory per `(repository, revision, config hash)`; atomic rename; checksums in
  `run-manifest.json`; a global index lists shard checksums. Content-derived `sample_id`s make re-processing
  idempotent.

## Splits and contamination

1. Group repositories **before any caret expansion**: forks join upstream; repositories whose file-hash sets
   overlap by Jaccard ≥ threshold merge. The selection already emits a shared `group` id (identical description /
   name+Jaccard), and merges Go standard-library / `golang.org/x` forks into one group so near-duplicate stdlib
   code never splits across train/eval/test.
2. Assign train/eval/test per group, never per sample or file (`split_group` carries the group).
3. Cross-repository exact dedup by file sha256 before sampling; near-duplicate files via MinHash over normalized
   token shingles (next step), same union-find.
4. Inside a repository, `project` (package dir) grouping is available for repo-internal ablations.

## Sampling caps and weighting

Per file (`max_samples_per_file`), per project/package, and a per-repository cap proportional to
`sqrt(accepted_lines)` so giant or copied codebases do not dominate. Test-code share measured separately
(`is_test`). Giant repos (size incl. history > threshold) are rejected at selection.

## Semantics at scale

* Baseline for all repositories: **syntax-only** (no code execution, scales with workers).
* Default semantic tier: **offline best_effort** (stdlib + in-module + vendor; external imports reason-coded
  partial). Measured on the pilot: see `docs/BENCHMARKS.md`.
* Trusted tier (future): a sandboxed module-aware pass (read the root `go.mod`, fetch modules from a mirror once
  into a per-trust-domain cache, no network during analysis) for allowlisted repositories — raises `resolved`
  rates on package-heavy code. Fallback for broken / missing-module / unsafe projects with reason codes.
* Caches keyed by `(repository, revision, package, build-context hash)`; semantic results keyed by
  `(sample_id, policy, extractor version)`; never shared across revisions.

## Throughput budget (from the pilot; see BENCHMARKS.md)

Syntax-only is CPU-bound and scales with workers (tens of thousands of samples/s per core on the pilot). Semantic
cost is dominated by per-package `go/types` checking plus stdlib source parsing (~150 samples/s, ~1 GB RSS on the
pilot). Planned optimization: cache each package's base compilation and bind the edited snapshot incrementally
instead of re-checking per caret (the Go analog of Roslyn speculative binding).

## Observability

Per-stage counters and timings are already emitted per run (`run-manifest.json`, `summary.json`). At scale:
aggregate per shard into a metrics table (repo, state, attempts, timings, sample counts, semantic rates, failures
by reason), graceful cancellation (`context.Context` is already threaded through the worker pool), bounded queues
(already `4×workers` files in flight).
