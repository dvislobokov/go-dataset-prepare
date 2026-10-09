# Prompt format (planned `flc-prompt/v1`, Go) — NOT YET IMPLEMENTED

Canonical records (`samples.jsonl`, `semantic.jsonl`) are model-agnostic and never contain a preformatted prompt.
A separate, versioned `render` step (milestone 3, analogous to the C# `flc-dataset render`) will turn them into
model-facing records. This document pins the intended contract so the canonical data already stores everything the
renderer needs.

```text
<|go|><|path|>internal/store/service.go
<|sem|>
EXPECT *store.User
RECV *Service
CALL (ctx context.Context, id int) (*store.User, error)
ARG ctx:context.Context id:int name:string svc:*Service
LOCAL cancel:context.CancelFunc err:error
RET (*store.User, error)
MEMBER GetUser(ctx context.Context, id int) (*User, error); PutUser(ctx context.Context, u *User) error
TYPE store.User: ID:int Name:string
<|code|>
	user, err := svc.store.<|complete|>
```
completion (loss only here): `GetUser(ctx, id)<|eol|>`

## Rules (intended)

* Order: stable metadata → semantic facts → most recent code. Code is cut **from the left on a line boundary**,
  never near the caret. Right context / suffix is never rendered (left-to-right model); it stays in the canonical
  sample for validation and future FIM.
* `<|sem|>` is omitted when no resolved facts exist (syntax-only samples, fallbacks).
* Stop token: a dedicated `<|eol|>`, not `\n` (BPE merges `\n` with the next line's indentation, making
  "stop at the first newline" ambiguous at the token level and mixing LF/CRLF). Multi-line mode (future) reserves
  `<|end_completion|>`. The model's inline suggestion MUST NOT emit the stop token.
* All `<|…|>` markers become dedicated special-token ids; they never appear as literal text in Go source.
* Budgets (UTF-16 chars until a tokenizer is pinned): `--max-code-chars`, `--max-semantic-chars`; semantic lines
  admitted by priority `EXPECT, RECV, CALL, ARG, LOCAL, RET, MEMBER, FIELD, TYPE, METHOD`, list items trimmed from
  the end.

## Vocabulary

`RET` enclosing func result type · `EXPECT` bound expected type at the caret · `ARG` params/named results/receiver ·
`LOCAL` locals before the caret · `FIELD`/`METHOD` receiver members (Go's "this") · `RECV` selector base type or
package · `MEMBER` accessible members of the receiver (all candidates, `+n` more) · `CALL` call signature at the
argument position · `TYPE T:` contract of a nearby repository type (leakage-safe: from in-scope facts only).

## Leakage policy

Facts come exclusively from the semantic sidecar computed on the target-free snapshot. `MEMBER`/`CALL` list all
accessible candidates (capped), never the one the target uses. The compact `prompt` field already emitted in
`semantic.jsonl` is a debug rendering, not the final training format.
