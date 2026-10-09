# Go FLC dataset builder — working notes for Claude Code

Go twin of https://github.com/dvislobokov/csharp-dataset-prepare (its `AGENTS.md` is the shared specification). Extraction,
validation and rendering are the Go CLI `goflc` (`cmd/goflc`, `internal/…`, standard library only); the bulk run is
`scripts/goflc_run.py`. Docs: `docs/DATASET_SPEC.md`, `docs/EXTRACTION_RULES.md`, `docs/PROMPT_FORMAT.md`, `docs/DEPLOY.md`.

## Communication and conventions
- Answer the user in Russian; code, identifiers, comments, docs and commit messages in English.
- Commit as the GitHub no-reply identity (`196775686+dvislobokov@users.noreply.github.com`), never the personal e-mail of the
  global git config; end messages with the Co-Authored-By / Claude-Session lines. Push only when the user asks.
- Never print or commit tokens (`/srv/flc/secrets/{HF_TOKEN,GITHUB_TOKEN}` on the server).
- Analysed code is never built or run: the semantic tier is offline `go/types` (stdlib from GOROOT, module packages from the
  checkout, external modules unresolved and reason-coded).
- Semantic engine changes need the equivalence check: facts of `original_scope` vs the `snapshot_typecheck` reference on real
  repositories (identical for unchanged engines, no leak-audit violations) plus `go test ./...`.

## Data products (Hugging Face, public: https://huggingface.co/datasets/dvislobokov/go-ml-complation)
- Configs `samples`, `semantic`, `prompts`, `corpus`, `repos`; every row carries repository, revision, path and licence (per-row
  attribution, `LICENSE.md`). Full run 2026-10-09: 8 907 repositories with samples, 8 931 in the corpus.
- `engine/go-16384/`: the corpus encoded with the plugin engine's `go-16384.bpe` (sha256 770a316b…, the tokenizer of the shipped
  go50m) in the idea-ml-completion training-shard format: `lm` 3.93 G tokens / 8 576 repos, `validation` 109 M, `test` 92 M
  (built by `csharp-dataset-prepare/scripts/encode_engine_shards.py --lang go`). Tokenizer study: keep the engine tokenizer.
- Splits are per repository group, not the engine's md5 folds.

## Server 161.104.53.71 (shared with the C# run; jobs as user `flc`)
- `/srv/flc-go/go-dataset` (this repo, rsync; `bin/goflc` built with `/opt/go`, `bin/goflc.prev` = previous build),
  state `/srv/flc-go/state/jobs.sqlite` and `/srv/flc-go/corpus/state.sqlite`, logs `/srv/flc-go/logs`, `/srv/flc-go/corpus/stdout.log`.
- Orchestrator: `scripts/goflc_run.py --manifest data/selection/selected.jsonl --jobs 80 --workers 2 --batch-samples 400000
  --upload-parallel 3 [--corpus-only --work/--out/--state/--logs under /srv/flc-go/corpus]`; SIGTERM stops gracefully, the same
  command resumes.
- Performance lessons (2026-10-09): set `GOMAXPROCS` = workers per goflc process (default 128 made ~36 processes keep ~420 threads
  runnable, load ~600); `original_scope` must cover carets in function literals (Ginkgo `var _ = Describe(…, func(){…})`, 5× faster);
  the HF upload is the tail — batches are merged in parallel and up to 3 commits upload concurrently.

## Next steps (shared plan with the C# project)
Dependency profile `DEPS` (external import paths used in ≥2 other files of the repository), the context block spec for the plugin
(go-psi side), training documents with/without context encoded with `go-16384.bpe`, evaluation with/without context, proxy model
on a rented GPU before plugin work.
