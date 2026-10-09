# Trust and security (Go)

Input repositories are untrusted. Analyzed code is **never built or executed** in any mode: no `go build`,
`go generate`, `go test`, cgo compilation, source-generator/`stringer`/`protoc` execution, or module download.

| Mode | Executes input code? | Network | Notes |
|---|---|---|---|
| `discover`, `extract --semantic none`, `validate` | No | No | Pure file reads + `go/scanner`/`go/parser`. Safe default. |
| `extract --semantic best_effort` | No | No | `go/types` with the offline importer: stdlib (builder's own `$GOROOT`), in-`go.mod`-module packages, and `./vendor`. `go/build.MatchFile` and `ImportDir` are driven with custom `ReadDir`/`OpenFile` hooks, which also stops `go/build` from shelling out to the `go` command. External imports resolve to type errors (reason-coded), never downloaded. |
| trusted tier (future) | would run `go list` / module download | Yes | **Not implemented.** Reserved for a sandboxed tier (read-only mount, no credentials, module mirror only, CPU/mem/time limits). Refused today. |

Never executed: the analyzed application or tests, build outputs, `go generate` tools, source generators
(`*.pb.go`, `*_gen.go`, `zz_generated*` are excluded, not regenerated), cgo C code.

## Other controls

* `pilot_clone.sh`: https only, `--depth 1` pinned shallow clone, `--recurse-submodules=no`, `GIT_TERMINAL_PROMPT=0`,
  no hooks run; the resolved SHA is recorded. Helper scripts live outside the cloned repositories and only read files.
* Discovery does not follow symlinks; the semantic importer's directory reader also skips symlinks so analysis
  cannot escape the checkout.
* License gate: unknown/non-allowlisted or conflicting licenses keep files out of the corpus unless
  `license.allow_unknown` is set explicitly.
* Secrets: file-level high-confidence formats (`secret_detected` → whole file skipped) and line-level literal-only
  credential assignments (caret-level `secret_detected`; UPPER_SNAKE env-var names and format templates excluded).
* GitHub token for metadata search is read from a file (`~/GITHUB_TOKEN`), sent only in the `Authorization` header,
  never printed or committed (`.gitignore` excludes `*TOKEN*`).
* Repository descriptions and code are treated as untrusted data: pattern-matched for selection, never executed or
  interpreted. The selection script runs with `python3 -I`.

## Isolation strategy for the future trusted tier at scale

Run any module-aware analysis per repository in a disposable container/VM: read-only source mount, no credentials,
network only to a module mirror during a one-shot fetch and none during analysis, CPU/memory/time limits, a fresh
module cache per trust domain. Repositories that fail or are not allowlisted fall back to the offline or syntax-only
modes with reason codes. See `docs/SCALE_PLAN.md`.
