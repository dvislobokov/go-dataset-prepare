# Prompt format `flc-prompt/v2` (Go, line mode) — implemented by `goflc render`

Same layout as the C# `flc-prompt/v2` (`/data/dataset-test/docs/PROMPT_FORMAT.md`); the language marker is `<|go|>`.
Canonical records (`samples.jsonl`, `semantic.jsonl`) are model-agnostic and never contain a preformatted prompt; the
versioned `goflc render` step produces `prompts.<split>.jsonl` (`flc-train/v1`: `prompt`, `completion`, `has_semantic`,
`code_truncated`, ...), `render-summary.json` and `preview.md`. `render --no-types` produces the `v1` layout.

```bash
goflc render --dataset DIR --out DIR/prompts --max-code-chars 4000 --max-semantic-chars 1500 [--preview 100]
```

Real record from the pilot (`jarcoal/httpmock`, code shortened here):

```text
<|go|><|path|>env_test.go
<|sem|>
EXPECT string
ARG t:*testing.T
LOCAL require
CALL os.Getenv(key string)->string @0 key:string
<|code|>
	defer func(orig string) {
		require.CmpNoError(os.Setenv(envVarName, orig))
	}(os.Getenv(<|complete|>
```
completion (loss only here): `envVarName))<|eol|>`

## Rules

* Order: stable metadata → semantic facts → most recent code. Code is cut **from the left on a line boundary**,
  never near the caret. Right context / suffix is never rendered (left-to-right model); it stays in the canonical
  sample for validation and future FIM.
* `<|sem|>` is omitted when no resolved facts exist (syntax-only samples, fallbacks).
* Stop token: a dedicated `<|eol|>`, not `\n` (BPE merges `\n` with the next line's indentation, making
  "stop at the first newline" ambiguous at the token level and mixing LF/CRLF). Multi-line mode (future) reserves
  `<|end_completion|>`. The model's inline suggestion MUST NOT emit the stop token.
* All `<|…|>` markers become dedicated special-token ids; they never appear as literal text in Go source.
* Budgets (UTF-16 chars until a tokenizer is pinned): `--max-code-chars` (4000), `--max-semantic-chars` (1500); at
  most 24 items per line; semantic lines admitted by priority `EXPECT, RECV, CALL, ARG, LOCAL, RET, MEMBER, FIELD,
  TYPE, METHOD`, list items trimmed from the end; output order is canonical
  (`RET EXPECT ARG LOCAL FIELD METHOD RECV MEMBER TYPE… CALL`).
* `<|sem|>` is rendered only for `resolved` / `partially_resolved` editor_snapshot records (fallbacks and
  `leak_audit` failures render without facts).

## Vocabulary

| Key | Meaning (Go) |
|---|---|
| `RET` | result type(s) of the enclosing function |
| `EXPECT T` | expected type at the caret (argument / return / assignment / `var x T =`) |
| `ARG` | parameters in scope: function params, named results, method receiver, closure params |
| `LOCAL` | locals declared before the caret and visible there (no type when it is from an unresolved module) |
| `FIELD` / `METHOD` | fields (incl. promoted) and method set of the enclosing method's receiver (Go's "this") |
| `RECV` | selector base after `.`: `package <import path>`, `type T` (method expression) or the value's type |
| `MEMBER` | accessible members after `.`: exported package members, or fields + methods of the receiver type |
| `CALL` | `callee(params)->results @i name:type` at an argument position (Go has no overloads; generic callees show the declared signature) |
| `TYPE T:` | contract of a nearby repository type reachable from in-scope locals/params/receiver fields |

Compact signatures: `Name(params)->results` (`->` omitted without results). Package-level declarations of the current
package (`package_members`) and imports are kept in the semantic sidecar but not rendered in v2.

## Leakage policy

Facts come exclusively from the semantic sidecar computed on the target-free snapshot. `MEMBER`/`CALL` list all
accessible candidates (capped), never the one the target uses. The compact `prompt` field already emitted in
`semantic.jsonl` is a debug rendering, not the final training format.
