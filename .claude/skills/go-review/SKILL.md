---
name: go-review
description: Review the Go code in this repo against CLAUDE.md conventions - runs gofmt, vet, staticcheck and the exported-name scan, then reads the changed (or named) packages for visibility, comments, errors, context and structure issues. Use when the user asks to review Go code, check Go best practices, or before committing a Go change.
---

# Go review

Review Go code in this repository against the conventions in `CLAUDE.md`.
Argument: a package path, a file, or nothing (defaults to the packages touched
by the working tree diff, or the whole module when the tree is clean).

## 1. Mechanical checks first

Run and report verbatim; a review that skips these is not a review:

```bash
gofmt -l .
make go-lint
.claude/skills/go-review/exported-scan.sh
```

The scan lists exported names never referenced outside their package. Keep
exported: enum constants of an exported type, `Err` sentinels, anything a
`cmd/` binary or test in another package uses. Everything else on the list is
a candidate to unexport.

## 2. Read the code

For each package in scope, read every non-test file fully and check, in this
order, citing `path:line` for each finding:

1. **Visibility.** Lowercase by default. Exported func returning or accepting
   an unexported type. Exported struct with unexported-typed fields. Test
   doubles in the production package. Missing one-line doc comment on an
   exported name.
2. **Comments.** Delete restatements of the code, comments naming identifiers
   that no longer exist, and descriptions of components that do not exist.
   Keep every comment that records a bug, a decision or a measurement.
3. **Errors.** `%w` with the operation; `errors.Is`/`errors.As`, never `==`;
   driver errors translated at the store boundary; nothing swallowed into a
   counter or fallback without a log line; `errors.New` for constant messages.
4. **Context.** First parameter, not stored; cleanup on shutdown uses
   `context.WithoutCancel` with a timeout; loops check `ctx.Err()`.
5. **Zero values and constructors.** Options struct above three dependencies;
   a zero-value struct is usable or the constructor applies defaults.
6. **Go specifics.** String slicing is bytes (`utf8` for runes); `append` into
   a shared backing array; `slices`/`maps`/`strings.Builder`/`min`/`max`/
   `for i := range n` over hand-rolled loops; `pgx.BeginFunc` for transactions;
   `rows.Close` then `rows.Err`.
7. **Tests.** `t.Context()`, `t.Helper()`, `t.Run` tables, `t.Parallel()` on
   pure tests, `//go:build integration` on tests that create or flush a
   database.

## 3. Report

Findings grouped by the seven headings, most severe first, each with
`path:line`, one sentence on the issue, one on the fix, and a severity of
`fix`, `consider` or `nit`. End with what the package does well, so the
next change keeps it. Do not edit code unless the user asked for fixes; when
they did, one commit per logical change and ask before each commit.
