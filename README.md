# Go Full-Line Completion dataset builder

A reproducible, semantic-aware pipeline that turns Go repositories into caret-based **full-line completion** (FLC)
training samples and a parallel source corpus. It mirrors the C# / Roslyn pipeline in `/data/dataset-test`
(same data contract and quality bar) but is implemented in Go on `go/scanner`, `go/parser`, `go/ast`, `go/types`.

**Status:** stage 2 — discovery, syntax caret extraction, a fast semantic stage (`go/types`, cached original
package + snapshot fallback, two visibility policies, per-record leakage audit), `render` (`flc-prompt/v2`), an
exact-reconstruction validator, candidate search + metadata-only selection, benchmarks, and the bulk orchestrator
`scripts/goflc_run.py` for the Hugging Face dataset `dvislobokov/go-ml-complation` (dry-run locally; deployment in
`docs/DEPLOY.md`). Input code is never built or executed. See `docs/PLAN.md`.

## Layout

```
cmd/goflc/              CLI: extract | discover | validate | render | config
internal/flc/           config, text model (byte/UTF-16), discovery, caret extraction, pipeline, thinning, validator
internal/semantic/      go/types offline loader, original_scope + snapshot engines, leakage audit
internal/render/        flc-prompt/v2 serialization
schemas/                flc-sample.v1.go, corpus-file.v1.go, flc-semantic.v1.go, run-manifest.v1 (JSON Schema)
configs/                go.base.json (built-in defaults), go.pilot.json, go.bulk.json (orchestrator base)
fixtures/basic/         synthetic module exercising Unicode, CRLF/LF, no-EOL, build tags, generated, secrets, vendor
docs/                   PLAN, DATASET_SPEC, EXTRACTION_RULES, SECURITY, SCALE_PLAN, BENCHMARKS, PROMPT_FORMAT, DEPLOY
scripts/                goflc_run.py (bulk orchestrator), github_search.py, benchmark.py, semantic_compare.py, pilot_clone.sh
data/go-search.jsonl    GitHub candidate metadata (12,409 repos)
data/selection/         metadata-only selection (script + report + selected/rejected jsonl)
data/repos/             pilot checkouts (gitignored)
artifacts/              datasets + benchmarks (gitignored except the committed pilot benchmark)
```

## Toolchain

Go is installed locally without sudo (official tarball, checksum verified) into `~/go-sdk/go` (Go 1.27.2):

```bash
export PATH=$HOME/go-sdk/go/bin:$PATH GOTOOLCHAIN=local GOPROXY=off
go version
```

`GOPROXY=off` works because the module has **no third-party dependencies**.

## Build & test

```bash
export PATH=$HOME/go-sdk/go/bin:$PATH GOTOOLCHAIN=local GOPROXY=off
go build -o bin/goflc ./cmd/goflc
go test ./...                 # unit + integration tests on synthetic fixtures (offline)
go vet ./...
```

## Extract, validate, benchmark

```bash
export PATH=$HOME/go-sdk/go/bin:$PATH GOTOOLCHAIN=local GOPROXY=off

# syntax-only (safe default; no code built or run)
./bin/goflc extract --repo data/repos/ttlcache --out artifacts/ttlcache \
  --repository-id github.com/jellydator/ttlcache --corpus --overwrite --workers 4

# semantic best-effort (go/types, offline: stdlib + in-module + vendor; external imports -> partial)
./bin/goflc extract --repo data/repos/ttlcache --out artifacts/ttlcache \
  --repository-id github.com/jellydator/ttlcache --semantic best_effort --overwrite --workers 4

# validate exact reconstruction (bytes + UTF-16), schema, and semantic leakage against the repo bytes
./bin/goflc validate --dataset artifacts/ttlcache --repo data/repos/ttlcache

# reproducible benchmark -> artifacts/benchmarks/benchmark.{json,md}
python3 -I scripts/benchmark.py --repo data/repos/ttlcache --repo data/repos/httpmock --workers 8 --subset 0.15

# reference engine for comparison (always type-check the target-free snapshot)
./bin/goflc extract ... --semantic best_effort --semantic-engine snapshot
python3 -I scripts/semantic_compare.py REF_DIR CANDIDATE_DIR

# model-facing prompts (flc-prompt/v2): prompts.<split>.jsonl + render-summary.json + preview.md
./bin/goflc render --dataset artifacts/ttlcache --out artifacts/ttlcache/prompts --max-code-chars 4000 --max-semantic-chars 1500

# corpus pass only (same filters; corpus.jsonl, no carets)
./bin/goflc discover --repo data/repos/ttlcache --out artifacts/ttlcache-corpus --overwrite

# print the effective default config
./bin/goflc config
```

Bulk-mode options (set per repository by the orchestrator): `sampling.keep_fraction`, `sampling.test_keep_fraction`,
`sampling.max_samples_per_repo` (hash-based, repository-wide, after dedup), `split.repository_split` /
`split.repository_group` (whole repository in one split), `--no-corpus`; stored contexts 6000 / 1000 chars.

## Bulk run (orchestrator)

```bash
# local dry run: 3 smallest selected repos, no upload, outputs kept (needs pyarrow; huggingface_hub only for upload)
python3 -m venv .venv && .venv/bin/pip install pyarrow huggingface_hub
python3 -I -c "import json; r=sorted(map(json.loads, open('data/selection/selected.jsonl')), key=lambda x:(x['size_kb'],x['repository_id']))[:3]; open('/tmp/m3.jsonl','w').write(''.join(json.dumps(x)+'\n' for x in r))"
.venv/bin/python -I scripts/goflc_run.py --manifest /tmp/m3.jsonl --no-upload --keep-local --jobs 3 --workers 2 \
  --work /tmp/goflc/work --out /tmp/goflc/out --state /tmp/goflc/state/jobs.sqlite --logs /tmp/goflc/logs \
  --go-root $HOME/go-sdk/go --github-token-file $HOME/GITHUB_TOKEN
```

Server deployment (Go into `/opt/go`, build, run, monitor): `docs/DEPLOY.md`.

## Candidate search & selection (metadata only, no cloning)

```bash
# collect GitHub candidates (token read from ~/GITHUB_TOKEN; never printed or committed)
python3 -I scripts/github_search.py --min-stars 100 --pushed-since 2024-10-09 --out data/go-search.jsonl

# deterministic metadata-only selection -> data/selection/{selected,rejected,...}.jsonl + report.md
python3 -I data/selection/selection_script.py

# shallow-clone a couple of small permissive repos for the pilot (https only, pinned SHA)
bash scripts/pilot_clone.sh
```

## Pilot (committed)

Stage 2 semantic speed-up and the engine agreement study: `docs/BENCHMARKS.md`.

2 small permissive repos (`jellydator/ttlcache` @ `230d2c7`, `jarcoal/httpmock` @ `ae9b916`): 42 files, ~3.8k
samples, both pass `validate` (exact reconstruction + 0 leakage violations). Numbers in `docs/BENCHMARKS.md` and
`artifacts/benchmarks/benchmark.json`.

## Safety

Input repositories are untrusted and never built or executed (no `go build/generate/test`, no cgo, no module
download). The semantic tier uses an offline `go/types` importer. See `docs/SECURITY.md`.
