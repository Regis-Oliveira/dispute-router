# Dispute Router

A study project: a miniature chargeback platform for learning Go, Angular, Redis
and AWS. Local only, one developer. `README.md` says what exists,
`docs/DECISIONS.md` says what was chosen against, `.spec/` holds plans for what
is not built yet. When a plan ships, its decisions move to `docs/` and the plan
is deleted.

## Commands

- `make go-test` — `go test ./... -race`. Tests needing Postgres, Redis or
  LocalStack skip themselves when `DATABASE_URL`, `REDIS_URL` or
  `AWS_ENDPOINT_URL` is unset.
- `make go-lint` — `go vet` and `staticcheck`. Both must be clean before a commit.
- `gofmt -l .` must print nothing.
- `make stack` explains which services to run and in which order.

## Go conventions

Visibility is per package, not per file. Everything under `internal/` is already
private to this module, so within it:

- Lowercase by default. Capitalize a name only when another package uses it.
  Enum constants of an exported type and `Err` sentinels stay exported.
- An exported function never returns or accepts an unexported type. An exported
  struct never has fields of unexported types.
- Test doubles live in an `xtest` subpackage (`internal/agent/agenttest`), not
  in the production package.
- Every exported name has a one-line doc comment starting with the name.

Comments explain *why*, never *what the code already says*. Keep the comment
that records the bug or decision behind a rule; delete the one that restates
the line below it, names an identifier that no longer exists, or describes a
component that does not exist. A floating paragraph belongs on a declaration.

- Errors: wrap with `%w` and the operation (`fmt.Errorf("load dispute %d: %w",
  id, err)`); `errors.New` when there is nothing to format; match sentinels with
  `errors.Is`, never `==`. Translate driver errors (`pgx.ErrNoRows`) into the
  package's own sentinel at the store boundary. Never swallow an error into a
  counter or a fallback without a log line.
- Config: a value that is present but unparseable is an error, not a default.
- `context.Context` is the first parameter, never stored in a struct. Cleanup
  that must run during shutdown uses `context.WithoutCancel` with a timeout.
- Constructors take an options struct once there are more than three
  dependencies. A zero-value struct must be usable or the constructor applies
  defaults.
- Transactions use `pgx.BeginFunc`. Shared inserts (`outbox`,
  `dispute_events`) go through one helper that takes a `pgx.Tx`.
- Vocabularies (dispute states, outcomes, actions) are typed string constants,
  not literals.
- Prefer `slices`, `maps`, `strings.Builder`, `for i := range n`, `min`/`max`
  over hand-rolled loops. Slicing a string slices bytes; use `utf8` for runes.
- Tests: `t.Context()` not `context.Background()`; `t.Helper()` in helpers;
  `t.Run` for tables; `t.Parallel()` on pure tests. Tests that create or flush
  a database carry `//go:build integration`.

## Commits

`type(scope): subject`, one logical change each, message long enough to carry
the reasoning. Ask before each commit. Nothing is pushed.
