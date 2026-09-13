# Go conventions: the reasoning

`CLAUDE.md` lists the rules this repository follows. This is why they are the
rules, taught from what an idiomatic-Go review of the whole module found on
2026-09-13 and from the forty commits that followed it. Every claim below names
the file it came from, so nothing here has to be taken on trust.

Worth stating before the list: the review found no build, format or `go vet`
problem in the module. Its 120 findings were about shape — where names live,
what a zero value means, which errors survive a package boundary, whether the
compiler was being told enough to be useful — plus a handful of real bugs that
those shapes had been hiding. None of them was about syntax. That is roughly
where the learning curve sits after the first month of Go.

---

## 1. Visibility is per package, not per file

This is the one that most needs unlearning coming from Node. A `const` that a
module does not export is invisible to every other file; the file is the unit.
In Go the unit is the directory. Every `.go` file in `internal/worker` is the
same package, and each one sees every lowercase name the others declare, with
no import and no export statement. `internal/worker/worker.go` reads the
`loaded` struct declared in `internal/worker/store.go` because they are the
same package, not because anything was shared.

So lowercase does not mean "private to me". It means "private to this package",
and the package is therefore the unit that gets reviewed, reasoned about and
kept honest. A file is a filing decision.

`internal/` is a second wall on top of that, enforced by the compiler and keyed
on the path rather than on any keyword: nothing outside the module can import
anything under `internal/`. The rule composes downward, which is the part worth
knowing. `cmd/internal/boot` can only be imported by packages under `cmd/`, and
that is exactly why the ten binaries could be given a shared bootstrap —
`Logger`, `Postgres`, `Redis`, `ServeHTTP` — without any of it becoming module
API that the rest of the code could reach for
(`cmd/internal/boot/boot.go`, `refactor(boot)`).

The consequence is that inside `internal/` a capitalized name is a promise to
the other packages of this module and nothing more. The review found about
seventy names capitalized with no reader outside their own package.
`signing.DefaultTolerance` and `ledger.ChargebackFeeMinor` were read only where
they were declared, so both went lowercase. Three kinds of name stayed
exported on purpose: enum constants of an exported type, `Err` sentinels — a
sentinel exists precisely so that a caller elsewhere can match it — and the
halves of an API that would look strange split up. `signing.Sign`, `Verify`,
`Compute` and `ParseHeader` are exported although only `VerifyAny` has an
outside caller, because a signing package that exposes verification and hides
signing is the odder shape.

### The bug shape: an exported function returning an unexported type

`worker.Store.Load` returned `loaded`, and `Store.Apply` took one. Both methods
were exported; the type was not. Go compiles that without complaint, and `go
doc` will print the signature, but a caller in another package cannot declare a
variable of that type, cannot write the struct literal, and cannot do anything
with the value except hand it straight back. The exported half of the API was
handing out something no consumer could name.

The fix is to decide which side is wrong. Here nothing outside the package
called either method, so both went down rather than the type coming up: they
are `load` and `apply` in `internal/worker/store.go` now. `internal/ingest/store.go`
had the identical asymmetry — `Record` and `RecordRuling` returned an
unexported result type — and resolved the same way, to `record` and
`recordRuling`.

The opposite resolution happened in the same phase, and it is worth seeing both.
`agent.Trace` was exported with fields of the unexported types `phase` and
`evidenceTrace`, so `cmd/agent/report.go` could not decode a trace into the
real struct and kept a forty-line private copy of it, synchronised by hand.
There the types deserved exporting: they are now `Phase` and `EvidenceTrace`,
`Run.Trace` is a `*Trace` instead of an `any`, and the hand-kept copy is gone.
The test to apply is not "which is smaller" but "is this type part of what the
package is for".

### Test doubles belong in their own package

A scripted fake sitting in the production package is exported surface that
ships with the binary, and it tempts production code into depending on it. The
double moved out to a subpackage, and `internal/agent/agenttest/draft.go` shows
the shape: `Draft` builds a written `agent.Draft` through
`agent.NewWrittenDraftForTest`, the one documented door past the deliberately
unexported `written` flag, so a test of the graders does not have to drive a
whole generator call to obtain something to grade. Fail-closed stays
fail-closed and the door says in its name what it is for.

The in-package tests keep a thin copy of the double anyway, in
`internal/agent/scripted_test.go`, and the reason is structural rather than
lazy. Go test files come in two flavours: `package agent` sees the package's
unexported names, `package agent_test` sees only its API. All seventeen test
files in that package reach for unexported names, so they are the first kind,
and the first kind cannot import `agenttest` because `agenttest` imports
`agent` — an import cycle.

One last echo of the same rule. The script that scans for over-exported names
originally excluded every path under a package's directory, so a name used only
by a subpackage looked unused; it proposed unexporting `agent.CheckCitations`
and `agent.NewWrittenDraftForTest` when `internal/agent/eval` and `agenttest`
call them. A subdirectory is a different package. That is the whole rule, and
even the tool written to enforce it got it wrong once.

---

## 2. The zero value has to work

Go has no constructors it can force you through. Any package that can name a
struct type can write `T{}`, and there is no way to mark a field as required.
The zero value is therefore part of the API whether it was designed or not, and
the only question is whether anyone has thought about what it means.

`api.Filters{}` rendered `ORDER BY  DESC` and `LIMIT 0`, because the defaults
lived in the HTTP query parser rather than in the type, and `internal/disputetools`
builds the struct directly rather than coming in through HTTP. The repair is
`normalize()` in `internal/api/filters.go`, applied by `ListDisputes` and
`StreamCSV` before either renders SQL: an empty or unknown sort becomes
`opened_at`, a zero limit becomes 50 and clamps at 200, a negative offset
becomes 0.

`worker.Consumer{}` was worse, because it worked. A zero `WaitTime` is a
zero-second long poll, which is a busy loop against SQS, and a zero
`MaxMessages` asks the queue for one message at a time. Nothing errors; the
bill does. `NewConsumer(ConsumerOptions)` in `internal/worker/events.go`
defaults them to 20 seconds and 10 messages and keeps the options unexported on
the struct, so the constructor is the only way in.

Notice that the two were fixed differently, and that the difference is the
interesting part. The filter normalises at the point of use, because a caller
writing a `Filters` literal with three fields set is doing something reasonable
and should not be punished for the other nine. The consumer closes the door,
because there is no reasonable `Consumer` literal — every field is a decision
about a remote service. Ask whether a hand-written zero-valued literal is a
thing a caller ought to be able to write, and the answer picks the mechanism.

The subtler version of the same trap is a zero value that means two different
things in two places. `Budget.MaxCostMicros == 0` meant "no ceiling" in the
assistant and in the eval runner, and "halt on the first turn" in the loop — so
a budget that looked unset was the only one that stopped the work. It means no
ceiling everywhere now.

---

## 3. A vocabulary written out by hand will drift

This is the strongest story in the repository, and it is worth following all
the way through because the lesson at the end is narrower than it first looks.

Migration `000003_agent.up.sql` added the `draft_ready` state on 2026-09-08. It
reached none of the places that enumerate states in prose rather than reading
the schema:

- the API's filter whitelist, so `?state=draft_ready` — the review queue, which
  is the one state an operator most wants to look at — answered 400;
- the dashboard's `DisputeState` union in
  `apps/dashboard/src/app/core/api.types.ts` and its filter control in
  `apps/dashboard/src/app/disputes/filter-bar.ts`;
- the JSON schema tag on the agent's `list_disputes` tool in
  `internal/disputetools/tools.go`, which told the model the state did not
  exist;
- the simulator's money invariant in `services/simulator/src/verify.ts`.

The last one is the one that mattered. The check named "money held equals the
chargebacks still open" listed `received`, `resolving` and `represented`, so
thirteen chargebacks sitting in the review queue held 1,184.38 USD that the
check counted as unheld — and the four merchant discrepancies it reported
summed to exactly that figure. `make verify` had been failing for five days.
The ledger was right the whole time and the check was stale. With `draft_ready`
included the query returns no mismatched rows and all eleven invariants pass.

The repair is `internal/dispute`: `type State string`, `type Kind string`, and
the constants that replace about sixty hand-typed literals across Go and the
SQL inside it. The values were checked against the `CHECK` constraints in
`db/migrations`, which are the authority — the package doc says so, and says
what follows from it: a constant added here without a migration is a write
Postgres refuses at runtime.

What is worth noticing is that every Go literal turned out to be in the
constraint, and every state in the constraint is written by some binary. The
vocabulary was consistent. It was simply unenforced, which is a different
problem and has a different lifespan: consistent-today survives exactly until
the next migration.

### What a named string type buys, and what it does not

It makes the compiler the extra reader. `dispute.StateDraftReady` can be found
by every tool that understands Go, a misspelling is a compile error at the
place it was typed rather than an empty result set an hour later, and a
function that takes a `dispute.State` cannot be handed a reason code by
accident.

It does not reach a struct tag, a TypeScript union, or a SQL string. Of the
five readers that missed `draft_ready`, two were TypeScript and one was a Go
struct tag — a string literal inside backticks that the compiler never inspects.
So the honest claim is not "typed constants would have prevented this bug". It
is that they would have caught the filter whitelist, and that the thing which
actually noticed the money was the invariant check failing. The value of that
check is that it failed; a green suite would have said nothing at all.

A constant also cannot be interpolated into SQL safely, so a state inside a
query became a bind parameter wherever the query already took them. Five
queries keep their literal, each with a comment explaining why: they sit on
partial indexes whose predicates name those exact literals, and the planner can
only use a partial index when it can prove the query implies the predicate,
which it cannot do about a bind parameter. `OpenDeadlines` in
`internal/worker/store.go` and the review queue query in
`internal/api/reviews.go` both carry that note. Binding them would have been the
tidier code and the slower query.

The same move ran seven more times through `internal/agent` — `Outcome`,
`Recommendation`, `Check`, `EvidenceStatus`, `Rule`, `eval.Rule` and
`RetrievalMethod`, the last of which had no constants at all, just `"vector"`,
`"lexical"`, `"none"` and `"failed"` compared by hand in two files. Because two
of the retyped fields carry `jsonschema` descriptions that the model reads and
the prompt fingerprint hashes, the derived schemas were dumped before and
after: byte-identical, because a named string type derives the same schema as a
string. That is the kind of check worth building the habit of — the type change
was provably invisible to the one consumer that could not be recompiled.

And there is a boundary the technique does not cross. `internal/api` still
spells `rejected` as a plain string, because `internal/agent` imports
`internal/api` and the reverse would be a cycle. The constant carries a comment
naming `agent.OutcomeRejected` as the other end and saying plainly that nothing
makes the compiler check the pair, and that the column's `CHECK` constraint is
the only thing that would refuse a bad write. Saying where the guarantee stops
is part of having one.

### The same instinct, applied to parameters

Every ledger posting function took `disputeID`, `merchantID` and `amountMinor`
as three adjacent `int64` parameters. Swapping two of them compiles, and posts
money to the wrong merchant — silently, in a double-entry ledger whose entire
purpose is that it cannot be silently wrong. They take an `Entry` now
(`internal/ledger/postings.go`), so every argument is named at the call site.
Each function keeps exactly one trailing string, which is safe for precisely
the reason the struct was needed: **a parameter with no same-typed neighbour has
nothing to swap with.** `direction` became a type with `debit` and `credit`
constants in the same commit, replacing eight loose literals.

---

## 4. Bytes are not runes, and a slice can alias

A Go string is a read-only slice of bytes. `range` over one yields runes, but
`s[i]` yields a byte and `s[:n]` cuts at a byte offset, and that inconsistency
is the whole trap. Masking an email with `local[:1]` cut the two-byte `é` in
`é@example.com` in half and produced invalid UTF-8 inside a JSON tool result.
`maskEmail` in `internal/disputetools/tools.go` uses
`utf8.DecodeRuneInString` now, and the comment beside it says what the byte
version cost.

The slice half of the lesson is less visible and more dangerous. A `[]T`
parameter is a window onto somebody else's backing array, and `append` may
write into it rather than allocating. `internal/api/decisions.go` appended the
limit and the offset straight onto `b.args` — the same backing array the count
query's arguments were still pointing at. That was safe only because the count
query had already returned, which is a fact about statement ordering rather than
about the code; two files away, `ListDisputes` runs its two queries
concurrently. The arguments are copied first now, `append([]any{}, b.args...)`,
and the comment records that the old version was correct by luck.

The embedding client's vector normaliser had a quieter version of the same
thing: it returned the caller's own slice when the vector summed to zero and a
fresh one otherwise, so whether the result aliased the input depended on the
data in it. It always clones now. A function that aliases sometimes has two
behaviours, and callers will learn the wrong one.

The rule that falls out: if you keep a slice you were handed, or grow it, copy
it first.

---

## 5. Errors carry meaning

`fmt.Errorf` with `%w` wraps rather than flattens, and `errors.Is` walks the
chain — which is why `==` is wrong even when it happens to work, since it only
ever matches the outermost error. `errors.As` is the same walk for a type
rather than a value, and it hands back the typed error so its fields can be
read.

The rule that does the most work here is translating a driver's sentinel at the
store boundary. `worker.Store.Load` returned `pgx.ErrNoRows` across the package
boundary, so `internal/worker/worker.go` imported the pgx driver for the single
purpose of comparing against it — every consumer of the store was made to know
which database library was underneath. `internal/worker/store.go` defines
`ErrNotFound` and returns
`fmt.Errorf("load dispute %d: %w", disputeID, ErrNotFound)`, the pool matches
that, and the pgx import is gone. `internal/api/store.go` already worked this
way. Note that this is the reason `Err` sentinels stay exported while almost
everything else in those packages went lowercase: a sentinel exists to be
matched from outside.

Then there are the errors that never arrive. `internal/config/config.go`'s
`integer` and `dur` returned the fallback when a variable was *present but
unparseable*, so `WORKER_LOCK_TTL=30` — a number with no unit — quietly became
the 30-second default and an operator's mistake never surfaced anywhere. That
is how a misconfiguration hides: not as a wrong value, but as the right-looking
default. Parse failures are collected now and returned from `Load` through
`errors.Join`, so a single restart reports every bad variable rather than the
first. Absent or empty still means the fallback; present and wrong is an error.

`errors.Join` earns its place again in `internal/dlq/redrive.go`, where `Replay`
used to increment `Failed` and drop the error, so an operator read "failed 3"
with no reason for any of them. `Stats.Errors` is a join of one wrapped error
per message, `cmd/dlq` prints them, and the exit code is non-zero. The general
form of both: never swallow an error into a counter or a fallback without a log
line.

`errors.As` shows up in `internal/ingest/handler.go`, where any error from
reading a capped request body was answered 413 — including a client simply
hanging up mid-upload, which is a 400 at worst and not the server's business at
all. Only `*http.MaxBytesError` is 413 now.

---

## 6. Context is not just a parameter

A `context.Context` is a cancellation signal that happens to travel as an
argument. Passing it everywhere is the easy half; the hard half is noticing the
two places where the cancellation is wrong.

The first is cleanup. The worker's deferred lock release and both of its
reschedule calls ran under the request context. On shutdown that context is
already cancelled, so every in-flight handler failed to release its lock and
left its dispute locked for the whole TTL — at the exact moment the lock is
least useful and most in the way, since the restarted worker then skips those
disputes as contended. `cleanupContext` in `internal/worker/worker.go` is
`context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)`:
`WithoutCancel` keeps the context's values and drops its cancellation, and the
two-second timeout is there because the context it came from no longer carries
a deadline and unbounded cleanup during a shutdown is its own bug.

The second is the opposite mistake — finishing early and not saying so.
`Run` in `internal/agent/backfill.go` broke out of its loop when the context was
done and returned `stats, nil`, so `cmd/embed` printed a success summary for a
run that had been cut short. It returns `stats, ctx.Err()` now. `ctx.Err()` is
how a function says "I stopped because I was told to"; `nil` says "I finished",
and the difference is the whole point of returning an error at all.

Two smaller consequences of the same thinking. A context is never stored in a
struct — it is scoped to a call, and a struct outlives calls. And a value on a
context that nothing reads is not documentation, it is dead weight: the HTTP
request id used to be stashed under a private key with no accessor, so the
lookup a reader would need did not exist. It was dropped, and the id lives on
the `X-Request-Id` header and in both log lines, which is where every reader was
already looking (`internal/httpx/middleware.go`).

Finally, a deadline belongs to a route rather than to a string comparison. The
API's timeout middleware used to match `/api/stream` by path to decide not to
apply one, because cutting a server-sent-events stream off at twenty seconds is
not a timeout, it is a bug. That held exactly until a second streaming route
would have inherited a deadline that severed it mid-stream, leaving no trace but
a client reconnecting every twenty seconds. `Routes()` wraps each route as it
registers it, so the one unbounded route is unbounded because of how it is
written down.

---

## 7. Comments that lie

Three shapes turned up, and they are not equally bad.

The first is a comment describing a component that does not exist. Several
sentences in `internal/agent` promised Bedrock in production and described
`InvokeModel`; the Bedrock provider had been built, judged untestable without
an AWS account, and deleted, with the argument for it preserved in
`docs/DECISIONS.md` — and the comments stayed behind pointing at nothing. An
`internal/outbox` comment promised a log publisher for phase 1 and SQS for
phase 4, when only the SQS one was ever built. The second shape is a comment
naming a plan step that was deleted, of which `internal/agent` had two.

Both of those merely waste a reader's time. The third does damage.
`internal/api/reviews.go` says that `ReviewFinding` mirrors `agent.Finding` and
that "the two shapes are pinned together by a test". No such test exists
anywhere in the module. The structs are redeclared rather than shared because
`internal/agent` imports `internal/api` and the reverse would be a cycle, so
nothing but a test *can* catch a field drifting between them — and the comment
is precisely the reason nobody ever went looking for one. A comment that
asserts a guarantee is load-bearing, and a false one is worse than silence,
because silence at least invites a check.

Hence the rule: keep the comment that records *why* — the bug behind a
constraint, the decision behind a shape, the reason a query spells out a
literal. Delete the one that restates the line below it, names an identifier
that no longer exists, or describes a component that does not.

There is a Go-specific placement rule underneath all of that. A doc comment is
made documentation by its position: directly above a declaration, starting with
the declared name. A floating paragraph after the last declaration in a file is
invisible to `go doc`, which means it is invisible to the reader most likely to
want it. The list of what the MCP server refuses to expose — read-only tools,
no generic `run_query`, no presigned evidence URLs, no unmasked customer emails
— sat in exactly that position, and is part of the package doc in
`internal/mcpserver/server.go` now. The note on what the worker's policy
deliberately does not do was attached to nothing at the end of
`internal/worker/rules.go`; it is `Decide`'s doc comment. And the `api` package
doc lived in `filters.go` claiming that every path is a GET and that the service
never writes, which stopped being true when the review queue got its decision
endpoint; it is `internal/api/doc.go` now, and it admits the write.

Reading `go doc ./internal/<pkg>` and asking whether the output is a usable
table of contents is the cheapest review this codebase has. It catches the
missing doc comment, the exported name that should not be, and the exported
function returning a type the reader cannot name — all three of section 1 — in
one pass.
