# Go Full-Line Completion dataset builder

A reproducible, semantic-aware pipeline that turns Go repositories into caret-based **full-line completion** (FLC)
training samples and a parallel source corpus. It mirrors the C# / Roslyn pipeline in `/data/dataset-test`
(same data contract and quality bar) but is implemented in Go on `go/scanner`, `go/parser`, `go/ast`, `go/types`.

**Status:** stage 1 — discovery, syntax caret extraction, a semantic prototype (`go/types`, two visibility
policies, leakage audit), an exact-reconstruction validator, candidate search + metadata-only selection, a
benchmark harness, and a 2-repo pilot. All offline; input code is never built or executed. See `docs/PLAN.md`.

## Layout

```
cmd/goflc/              CLI: extract | validate | config
internal/flc/           config, text model (byte/UTF-16), discovery, caret extraction, pipeline, validator
internal/semantic/      go/types offline loader, caret facts, two visibility policies, leakage audit
schemas/                flc-sample.v1.go, corpus-file.v1.go, flc-semantic.v1.go, run-manifest.v1 (JSON Schema)
configs/                go.base.json (built-in defaults), go.pilot.json
fixtures/basic/         synthetic module exercising Unicode, CRLF/LF, no-EOL, build tags, generated, secrets, vendor
docs/                   PLAN, DATASET_SPEC, EXTRACTION_RULES, SECURITY, SCALE_PLAN, BENCHMARKS, PROMPT_FORMAT
scripts/                github_search.py, benchmark.py, pilot_clone.sh
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

# print the effective default config
./bin/goflc config
```

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

2 small permissive repos (`jellydator/ttlcache` @ `230d2c7`, `jarcoal/httpmock` @ `ae9b916`): 42 files, ~3.8k
samples, both pass `validate` (exact reconstruction + 0 leakage violations). Numbers in `docs/BENCHMARKS.md` and
`artifacts/benchmarks/benchmark.json`.

## Safety

Input repositories are untrusted and never built or executed (no `go build/generate/test`, no cgo, no module
download). The semantic tier uses an offline `go/types` importer. See `docs/SECURITY.md`.
