# Go review: fixes, visibility, structure, observability

Status: **Phases 0 and 1 done (2026-09-13); 5.2 built early.** Next: Phase 2. Source: a full
idiomatic-Go review of the module on 2026-09-13 (gofmt, vet, build clean;
staticcheck one test nit; 120 findings across three package slices).

Each item names the file, the change, and how it is verified. Phases are
ordered so that every later change lands on code whose behaviour is already
pinned: bugs first, then names and comments (mechanical, no behaviour change),
then structure (behaviour-preserving but wide), then the tests that the
structure makes possible, then the documents, then observability last because
it is an addition rather than a correction.

Commit per item, `type(scope): subject`, and ask before each commit. Nothing is
pushed. Items marked **DECIDE** change an API shape or add a dependency and
want the developer's sign-off before they are built.

The comment rule applied throughout, from `CLAUDE.md`: keep the comment that
records *why*; delete the one that restates the line below it, names an
identifier that no longer exists, or describes a component that does not
exist. Every exported name gets its one-line doc comment as its package is
touched, so no phase is "add comments" on its own.

---

## Phase 0 — baseline — DONE

**0.1 `CLAUDE.md`.** The conventions the rest of this plan enforces, so that
future sessions apply them without re-reading this file.

**0.2 staticcheck in `make go-lint`.** `Makefile`. Pinned to the module version
(`honnef.co/go/tools@v0.8.1`, staticcheck 2026.2.1), run through `go run` so
nothing has to be installed. Verify: `make go-lint` prints only the
`assist_test.go` SA4000 nit, which 1.10 removes.

**0.3 This plan**, linked from `README.md`.

---

## Phase 1 — bugs — DONE

Behaviour changes, each small, each with a test where one is cheap.

**1.1 Config refuses what it cannot parse.** `internal/config/config.go`
- `integer` and `dur` return the fallback when the variable is *present but
  unparseable* (`WORKER_LOCK_TTL=30` silently becomes the default). Collect
  parse failures with `errors.Join` and return them from `Load`.
- While there: `os.IsNotExist` → `errors.Is(err, fs.ErrNotExist)`;
  `fmt.Errorf` with no verbs → `errors.New`.
- Verify: a `config_test.go` with one bad duration and one bad integer.

**1.2 One `.env` lookup, one AWS config.** `internal/config/config.go`,
`internal/awsx/awsx.go`, every `cmd/*/main.go`
- `.env` is found three ways today (`dotenvPath()` duplicated in `ingest` and
  `mcp`; a literal `".env"` in `api`, `worker`, `dlq`; a raw
  `os.Getenv("DOTENV_PATH")` in the other five). Move the lookup into
  `config.Load()` with no parameter: `DOTENV_PATH`, else `.env`.
- `awsx.Config.Endpoint` is documented but never read; the constructors take
  the endpoint again as a parameter, and the `awsx.Config` literal is written
  six times with two copies missing the field. `Load` applies
  `config.WithBaseEndpoint`; the three constructors lose their `endpoint`
  parameter, and `S3` turns on path style when `aws.Config.BaseEndpoint` is
  set, so the LocalStack decision lives in `awsx` alone; `config.Config` gains
  `AWS() awsx.Config` so the mapping exists once.
- Verify: `go build ./...`; `make aws-status` still reaches LocalStack.

**1.3 The worker pool waits for its own handlers.** `internal/worker/worker.go`,
`cmd/worker/main.go`
- The review's premise was wrong: `Pool.Run` already returns only after every
  handler has (`handle` runs inside `drainOnce`'s errgroup, which is waited
  synchronously). The 200 ms sleep in `main` dated from the worker's first
  commit and guarded nothing. Deleted; the guarantee is now stated on `Run`'s
  doc comment so nobody re-adds the sleep.
- `worker.NewDeadlines(rdb)` was built twice; built once.
- Verify: `go test ./internal/worker/` with `REDIS_URL` set.

**1.4 A cancelled backfill says so.** `internal/agent/backfill.go`
- On `ctx.Err() != nil` the loop breaks and returns `stats, nil`, so
  `cmd/embed` reports success for a run that was cut short. Return
  `stats, ctx.Err()`; the deferred `Remaining` recompute already uses
  `WithoutCancel`, so the numbers stay true.
- Verify: a scripted test that cancels after the first batch.

**1.5 Zero values that work.** `internal/api/filters.go`,
`internal/worker/events.go`
- `Filters{}` renders `ORDER BY  DESC` and `LIMIT 0`; `ListDisputes` applies
  defaults through a `normalize()` step so external callers cannot build an
  unusable value.
- `worker.Consumer{}` busy-polls SQS (`WaitTime` zero) and asks for one message.
  Add `NewConsumer(ConsumerOptions)` applying 10 messages / 20 s, matching the
  `NewPool(Options)` shape beside it.
- Verify: `go test ./internal/api/ ./internal/worker/`.

**1.6 Replay reports its failures.** `internal/dlq/redrive.go` (built with a
LocalStack-gated `redrive_test.go` that pins `Skipped:1`, since `Redriver`
holds a concrete SQS client)
- `Replay` counts `Failed++` and drops the error. `Stats` gains `Errors error`
  built with `errors.Join`, printed by `cmd/dlq`.
- A skipped message is released with visibility 0 and re-received on the next
  batch, so one over-redriven message is counted `Skipped` until `limit` is
  reached. Release skipped messages after the loop, as `Peek` already does.
- `Depth` wraps the `Atoi` error with the queue URL.
- Verify: `make dlq-replay` dry run; the SQS-gated test in `internal/worker`
  is the only live harness, so add a unit test around the skip bookkeeping if
  it can be isolated, otherwise document the manual check.

**1.7 The ingest handler answers the right status.** `internal/ingest/handler.go`
- Any error from `io.ReadAll(http.MaxBytesReader(...))`, including a client
  hanging up, is answered 413. `errors.As(err, new(*http.MaxBytesError))` is
  413; everything else is 400.
- `Now func() time.Time` is added to `HandlerOptions` (the hook exists, nothing
  can set it) so 1.8 and Phase 4 can pin the clock.
- Verify: Phase 4.1's handler test covers both.

**1.8 Driver errors stay at the store boundary.** `internal/worker/store.go`,
`internal/worker/worker.go`, `cmd/eval/main.go`
- `Store.Load` returns `pgx.ErrNoRows` across the package boundary and
  `worker.go` imports pgx only to `errors.Is` it. Define `worker.ErrNotFound`,
  translate in `Load`, drop the import.
- `cmd/eval` compares `err == pgx.ErrNoRows`; use `errors.Is`.
- Verify: `go build ./...`; `go test ./internal/worker/`.

**1.9 Bytes are not runes.** `internal/disputetools/tools.go`,
`internal/agent/generate.go`
- `local[:1]` when masking an email slices bytes; an address starting with a
  multibyte rune produces invalid UTF-8 in a JSON tool result. Use
  `utf8.DecodeRuneInString`.
- The hand-rolled `contains` in `generate.go` is `slices.Contains`.
- Verify: a table case with `é@example.com` in `tools_test.go`.

**1.10 Small correctness nits.** One commit.
- `internal/httpx/middleware.go`: the recover block re-panics
  `http.ErrAbortHandler` instead of logging it as a crash.
- `internal/ledger/postings.go`: JSON metadata built with `%q` becomes
  `json.Marshal`; `%q` is not JSON escaping.
- `internal/agent/assist_test.go:155`: the stability test compares two locals,
  which is what staticcheck's SA4000 is asking for.
- `internal/agent/evidence.go`: `object.Body.Close()` is deferred inside a
  small func.
- `cmd/dlq/main.go`: `flag.ExitOnError` makes the `Parse` error check dead
  code; use `ContinueOnError` and return the error through `run`. Add
  `signal.NotifyContext`, the one main without it.
- Verify: `make go-lint` is fully clean; `go test ./...`.

---

## Phase 2 — names and comments

No behaviour change. One commit per package so each is reviewable.

**2.1 Unexport what only the package uses.** From the scan in the review
(names Capitalized but never referenced outside their package, excluding enum
constants of exported types and `Err` sentinels, which stay):
- `signing`: `DefaultTolerance` (tests only).
- `ingest`: `Decode`, `DecodeRuling`, `PeekType`, `MaxClaimBytes`,
  `RecordResult`; rename `Decode` → `decodeDispute` for symmetry.
- `api`: `ParseFilters` (if the handler is its only caller).
- `worker`: `Decide` stays exported (it is the documented policy function and
  `rules_test` reads better against a public name) — **DECIDE**; `DeadlineKey`
  goes lowercase.
- `agent`: `BaseRates`, `CheckCitations`, `Lexical` (rename `byText` to
  `lexical` and delete the wrapper), `OutcomeFailed` (never used: delete).
- `eval`: `GradeDraft`, `Graders`, `Instructed`, `Passed` (keep whichever
  `cmd/eval` calls; the scan says none).
- Verify: `go build ./... && go vet ./...` — the compiler is the test.

**2.2 Exported edges that leak unexported types.** `internal/worker/store.go`,
`internal/agent/assist.go`, `internal/agent/store.go`
- `Store.Load` returns `loaded` and `Apply` accepts it; only `Pool` calls them.
  Unexport both (`load`, `apply`) — **DECIDE** between that and exporting
  `Loaded`.
- `agent.Trace` has fields of types `phase` and `evidenceTrace`. Export them
  as `Phase` and `EvidenceTrace`; `Run.Trace any` becomes `*Trace`.
- Verify: `cmd/agent/report.go` can then decode a trace without its private
  `runRow` copy; do that in the same commit.

**2.3 Test doubles out of the production package.** New
`internal/agent/agenttest` with `ScriptedCompleter`, `Says`, `Calls`,
`Truncated`, and a `Draft(...)` constructor so `eval` tests no longer drive the
real generator to obtain a written draft. **DECIDE**: `Draft.written` is
unexported on purpose (fail-closed); the constructor would live in
`internal/agent` behind an `agenttest`-only door, e.g. an exported
`NewWrittenDraftForTest` that `agenttest` wraps. Verify: `go test
./internal/agent/...`.

**2.4 Doc comments and comment cleanup, per package.** Add the one-line
`// Name ...` to every exported name (116 missing across `internal/`). Apply
the comment rule: delete restatements, fix names in comments that drifted
(`assist.go` `MaxCostMicros`/`MaxAttempts`; `grade.go` "Grade runs" on
`GradeDraft`), remove the "Bedrock in production" sentences in `loop.go`,
`tools.go`, `anthropic.go` (no Bedrock completer exists), fold the floating
paragraph at the end of `worker/rules.go` into `Decide`'s comment, move the
`api` package doc from `filters.go` to `doc.go` and fix "every path is a GET".
Verify: `go vet ./...`; `go doc ./internal/<pkg>` reads as a table of contents.

**2.5 Names that mislead.** `httpx.Middleware` → `httpx.Observe`;
`cmd/eval` `print` → `printReport` (shadows a builtin); `api` `TookMs` →
`TookMS`; `disputetools` `OriginalChargeText`/`OriginalCharge` →
`OriginalCharge`/`OriginalChargeMinor` (JSON names unchanged — the dashboard
reads them).

---

## Phase 3 — structure

Behaviour-preserving. Each item is one commit; the order is by how many later
items depend on it.

**3.1 One insert for `outbox`, one for `dispute_events`.** `internal/outbox`,
new `internal/events` (or a function in `internal/ingest`… **DECIDE** which
package owns `dispute_events`; the proposal is a small `internal/events`
package with `Record(ctx, tx, Event)`), callers in `api/reviews.go`,
`ingest/store.go`, `worker/store.go`, `agent/store.go`.
- Four hand-written inserts with drifting shapes (`api` omits `occurred_at`;
  `ingest` casts `::jsonb` from a string, `api` passes `[]byte` uncast).
- Verify: the live store tests in all four packages; `make flow`.

**3.2 One transaction idiom.** `pgx.BeginFunc` everywhere. `worker/store.go`
keeps its testable `applyTx` body; `ingest/store.go`'s "commit the failure"
path returns nil from the body and carries `ErrUnknownTransaction` out in a
captured variable. Extract `insertDelivery` from the duplicated block in
`Record`/`RecordRuling`. Verify: `go test ./internal/ingest/ ./internal/worker/`.

**3.3 Typed vocabularies.** New `internal/dispute` with `type State string`
and the constants the SQL and Go both use (~60 literals today), following the
`worker.Action` shape. In `internal/agent`: `type Outcome string`,
`type Check string`, `type Recommendation string`, `type RetrievalMethod
string`. Verify: the compiler; `grep -rn '"draft_ready"' internal cmd` finds
only the constant.

**3.4 A boot package for the mains.** New `cmd/internal/boot` (Go restricts it
to `cmd/`): `Logger(json bool)`, `Postgres(ctx, url)`, `Redis(ctx, url)`,
`ServeHTTP(ctx, *http.Server, grace)`. Each main shrinks by ~40 lines; the
stdout-JSON vs stderr-text logger choice becomes explicit per binary.
`writeJSON`/`writeError`/`Ping` duplicated between `api` and `ingest` move to
`internal/httpx`. Verify: every binary starts (`make stack` order).

**3.5 Config grouped by consumer.** `config.Config` gains `Ingest`, `Worker`,
`Agent`, `AWS` sub-structs; the `SQS_QUEUE_URL`/`S3_EVIDENCE_BUCKET`
requirement moves to the binaries that use them (`ask`, `embed`, `retrieval`,
`mcp` currently need AWS variables they never read). `loadDotEnv` stops
calling `os.Setenv`. **DECIDE**: this touches every main; it can wait.

**3.6 Ledger entries as a struct.** `internal/ledger/postings.go`. Three
adjacent `int64` parameters (`disputeID, merchantID, amountMinor`) swap
silently; `Entry{DisputeID, MerchantID, AmountMinor, Currency, At}` and
`type direction string`. Verify: `go test ./internal/worker/` (the ledger's
only live test path) and `make verify`.

**3.7 Generator and verifier share one call.** `internal/agent/generate.go`,
`verify.go`. The two 70-line "render, derive schema, forced tool call, price,
check `max_tokens`, unmarshal" flows become `callTool[T any]`. JSON schemas are
derived once (`sync.OnceValue`) instead of on every call, and one helper
replaces `mustSchema`/`schemaFor`/inline. Verify: `go test ./internal/agent/`
scripted tests are unchanged.

**3.8 Split `internal/agent`.** **DECIDE.** `internal/llm` takes the wire types
and the two clients (`Completer`, `Request`, `Response`, `ContentBlock`,
`Usage`, `Pricing`, `Anthropic`, `Embedder`, `Voyage`). `cmd/ask` then
imports `llm` and the loop; `cmd/embed` imports `llm` and the backfill.
`disputetools.Set` takes a four-method interface instead of `*api.Store`, and
`agent.NewRegistry` takes `[]disputetools.Definition`, so both packages'
tests can run without a database. Largest item in the plan; last in the phase
so it lands on settled code.

**3.9 Smaller shapes.** One commit each, any order:
- `facts.go` `WithPrecedent`/`WithBaseRates` become fields on a
  `FactSourceOptions` struct with `evidence != nil` validated in the
  constructor.
- `eval.Runner`: exported fields *or* setters, not both (keep the fields).
- `worker.Locks` returns a `*Lock` with `Release(ctx)`; release and
  `rescheduleSoon` use `context.WithoutCancel` with a 2 s timeout so shutdown
  does not leave locks for the TTL.
- `ingest.Handler.ServeHTTP` (190 lines) splits into `authenticate` and
  per-type `record` functions.
- `api.Timeout` wraps per route instead of matching `/api/stream` by string;
  `Routes()` returns `http.Handler`.
- `api/decisions.go` reuses `builder` from `filters.go` and copies `args`
  before appending (slice aliasing).
- `Budget.MaxCostMicros == 0` means "no ceiling" everywhere (today it halts on
  the first turn in `loop.go` and means no ceiling in `assist.go`/`eval`).
- Two Anthropic/Voyage "retryable" signals become one `apiError` with
  `Retryable()`; `Retry-After` honoured; the "fixed backoff" comment matches
  the linear code or the code becomes fixed.
- `debugx`: `syscall.Getrusage` behind `//go:build unix` with a stub; the
  150-line HTML page moves to `live.html` with `//go:embed`.

---

## Phase 4 — tests the structure makes possible

**4.1 The ingest pipeline is pinned.** `internal/ingest/handler_test.go`. An
`httptest` test over a fake `secrets.Resolver`, the injected clock, and the
`REDIS_URL` gate, covering the documented order (IP limit → body cap →
signature → merchant limit → idempotency), "unknown merchant and bad signature
both 401", 413 vs 400 from 1.7.

**4.2 Assertions that name the contract.** `api/store_test.go`
`TestDisputeNotFound` asserts `errors.Is(err, ErrNotFound)`. `decideReview`'s
status mapping and `parseFilters` get table tests.

**4.3 Test hygiene.** `t.Context()` replaces `context.Background()` (about 30
sites); `t.Parallel()` on the pure tests (`rules_test`, `event_test`,
`evidence_test`, `cors_test`, `render_test`, `grade_test`, `loop_test`);
`money/format_test.go` uses `t.Run`; `mcpserver/protocol_test.go` drops
`containsRune` and the `var _ = time.Second` import keeper.

**4.4 Destructive tests carry a tag.** `//go:build integration` on
`api/reviews_test.go` (creates and drops a database) and `worker/redis_test.go`
(`FlushDB` on DB 15); `make go-test-integration` runs them. The other live
tests keep env-var gating: they only read. **DECIDE** whether to tag all live
tests for uniformity.

---

## Phase 5 — documents

**5.1 `docs/go-conventions.md`.** The study document: what the review taught
about Go, with the examples from this codebase — package scope vs file scope,
`internal/`, exported-returns-unexported, zero values, slice aliasing, byte vs
rune slicing, Go 1.22 loop variables, `%w: %w`, `context.WithoutCancel`,
`t.Context()`. English, like `DECISIONS.md`; a Portuguese section can be added
to `conceitos-pt.md` afterwards. **DECIDE** the language.

**5.2 `/go-review` skill — DONE (built in Phase 0).**
`.claude/skills/go-review/SKILL.md`: runs `gofmt -l`, `make go-lint`, the
exported-name scan (`exported-scan.sh` beside the skill), then reviews the
diff against `CLAUDE.md`. On-demand, not automatic.

**5.3 Close the plan.** The decisions worth keeping (why `pgx.BeginFunc`, why
a boot package, why typed states, why Sentry) move to `docs/DECISIONS.md`;
`README.md` links `docs/go-conventions.md`; this file is deleted.

---

## Phase 6 — observability — DECIDE

What the free tiers are, checked 2026-09-13:
- **Sentry Developer plan**: free; 5,000 errors and 10,000 performance units a
  month, 30-day retention, one user, plus small allowances for logs, spans,
  replays and one cron/uptime monitor. Stops accepting events at the cap, no
  overage charge.
- **Datadog free plan**: infrastructure metrics for up to five hosts with one
  day of retention. No APM, no log management, no RUM. Those are paid (14-day
  trial only).

So Sentry is the one that can be tried on this project at no cost, and it
covers both halves the question asked about: Go services and the Angular
dashboard.

**6.1 Go.** `github.com/getsentry/sentry-go`. One `boot.Sentry(dsn)` in the
boot package from 3.4, called by every main; a `slog` handler
(`sentry-go/slog`) so `logger.Error` becomes a Sentry event with the
structured fields attached; `sentryhttp` middleware in `httpx` beside
`Observe` so a recovered panic is reported with the request id; the worker
wraps each dispute handler in `sentry.Recover`-style capture with the dispute
id as a tag. Unset DSN means disabled, so nothing changes locally by default.

**6.2 Angular.** `@sentry/angular`: `Sentry.init` in `main.ts`, the
`ErrorHandler` provider, `TraceService` for route timing. The dashboard's
`environment.ts` carries the DSN; empty means off.

**6.3 The vendor-neutral alternative.** OpenTelemetry (`go.opentelemetry.io/otel`
+ the OTLP exporter) sends traces and metrics to any backend, including Sentry
and a local Jaeger/Grafana. More to learn, no free hosted backend needed. Worth
a section in `docs/go-conventions.md` explaining the trade-off even if Sentry
is what gets wired.

Verify: a deliberate `panic` in a handler and a `logger.Error` in the worker
both appear in the Sentry project; the dashboard's error boundary reports a
thrown error. Then the counts go in `docs/measurements/`.
