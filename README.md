# Dispute Router

A miniature chargeback platform, built to learn **Go, Angular, Redis and AWS** on top of a
stack I already know (Node, TypeScript, PostgreSQL).

Merchants take card payments. Some cardholders dispute them. Every dispute arrives with a
deadline attached, and the entire product is the race between that deadline and a decision:
refund now and close it, or fight it with evidence. Get the decision wrong and it costs
money; miss the deadline entirely and the option is gone.

That domain is chosen on purpose. It makes each unfamiliar piece of the stack *necessary*
rather than decorative:

| Piece | What it is actually load-bearing for |
| --- | --- |
| **Go** | Webhook ingestion and the worker pool that drains the deadline queue. |
| **PostgreSQL** | An append-only event log and a double-entry ledger where correctness is enforced by the database, not by application code. |
| **Redis** | Idempotency, distributed locks, per-merchant rate limiting, and a sorted set used as a deadline timer. Four distinct patterns, none of them caching. |
| **Angular** | An operations dashboard over half a million rows: server-side pagination, filters, exports, charts, a live feed. |
| **AWS** | SQS for the queue, S3 for evidence uploads, ECS for the services. |
| **Node/TypeScript** | The fake payment processor that generates the world. |

## Status

**Phases 0 and 1 are written.** Phase 0 is verified end to end; Phase 1 needs a Go
toolchain on this machine before it can be built and run.

| Phase | What | State |
| --- | --- | --- |
| 0 | Postgres schema, double-entry ledger, Node simulator | done |
| 1 | Go ingest service: HMAC verification, Redis idempotency, outbox | written, unbuilt |
| 2 | Angular dashboard over the seeded data | next |
| 3 | Redis deadline timers, Go worker pool, dispute state machine | |
| 4 | Swap the homegrown queue for SQS, S3 evidence uploads, deploy | |

Phase 4 comes last on purpose. Building the queue by hand first and *then* migrating it to
SQS teaches more than reaching for SQS on day one.

## Running it

Requires Docker and Node 20.11+ (the package pins 22 via Volta; `import.meta` path
resolution and `fetch` both need a recent runtime). Nothing else — Postgres and Redis run in
containers, so there is no local `psql` or `redis-cli` dependency.

```bash
cp .env.example .env
make up                    # postgres on :5433, redis on :6379
cd services/simulator && npm install && cd -
make migrate
make seed                  # ~500k transactions; SEED_TRANSACTIONS=20000 make seed for a fast one
make verify                # asserts the money invariants, prints a summary
```

`make psql` opens a shell on the database. `make reset` destroys the volumes and starts over.

The generator is deterministic: every row is a pure function of `SEED`. Reproducing a bug is
`SEED=42 make seed`, not a database dump.

## The parts worth reading

### `db/migrations/000002_ledger.up.sql`

The double-entry ledger, and the reason this project exists. Nothing "adjusts a balance".
Money moves by writing a balanced journal entry, and three properties are guaranteed by
the database rather than by whoever wrote the last service:

1. **Every journal entry balances.** A `DEFERRABLE INITIALLY DEFERRED` constraint trigger
   checks debits against credits at `COMMIT`, so postings can be inserted in any order but
   an unbalanced entry can never be committed.
2. **A posting's currency matches its account's.** Enforced by a composite foreign key
   against `ledger_accounts (id, currency)` — not by a service remembering to check.
3. **Postings are immutable.** `UPDATE` and `DELETE` raise. A mistake is corrected by
   posting a reversing entry, which leaves both the error and the fix in the audit trail.

Balances are a view, never a stored column, so there is no cached number to drift.

`ledger_transactions.external_ref` is the idempotency key for money: `dispute:1234:refund`
can be posted exactly once. A worker that retries after a crash, or two workers racing the
same dispute, cannot double-refund — the unique violation is the *successful* outcome.

### `db/migrations/000001_init.up.sql`

The domain. Two details that shape everything downstream:

- **Money is `BIGINT` minor units plus a currency column.** No `NUMERIC`, no floats, no
  bare number that means "dollars". Amounts are positive; direction lives in the ledger.
- **`disputes.deadline_at` with a partial index** on the still-open states. The deadline
  sweeper's index stays small no matter how much history piles up behind it.

Disputes arrive in two kinds. An **alert** is a pre-dispute warning with a day or two to
refund before a chargeback is ever filed — that is the window worth racing. A **chargeback**
is the real thing: the money is already gone and you are arguing to get it back.

### `services/simulator/`

The fake processor. `seed` bulk-loads history with `COPY ... FROM STDIN` and derives the
ledger set-based in SQL, because posting half a million journal entries row-by-row from the
application takes minutes instead of seconds. `emit` streams HMAC-signed webhooks at the
ingest service that Phase 1 will build.

`emit --replay` sends every event twice. Until Phase 1's Redis dedupe exists, that is how
you watch one event become two disputes.

A full seed is 500,000 transactions, ~13,000 disputes and ~1,020,000 ledger postings in
about 38 seconds.

**The first version took 508 seconds**, and the reason is worth keeping. `COPY` does not
update planner statistics. The refund backfill ran immediately after the bulk load, so the
planner still believed `transactions` was empty, picked a nested loop against a 500,000-row
table, and spent 466 of those 508 seconds on an `UPDATE` touching 6,771 rows. Analysing
each table as soon as it is loaded — rather than once at the end — took that step from
466.5s to 0.4s. The plan was never wrong; the statistics were missing.

## `services/ingest/` — the Go service

`POST /webhooks/processor` takes a signed dispute webhook and turns it into a row. The
order of its steps is the design, not an accident:

1. A per-IP token bucket and a 64KB body cap, because everything after them costs a
   database round trip.
2. Parse the body. This has to happen before the signature check, because the merchant id
   that selects the verification secret is *inside* the body — so nothing from the parse is
   acted on until step 4; it only chooses which key to check against.
3. **An unknown merchant and a bad signature return the same 401.** Different answers turn
   the endpoint into a merchant-id oracle.
4. Only once the signature holds does the request get to spend the merchant's rate-limit
   budget or reach the write path.

Two details worth the reading time:

**The idempotency key comes from the signed body, never the `Idempotency-Key` header.**
That header isn't covered by the signature, so keying off it would let anyone suppress a
real event by guessing an id. Redis `SETNX` is the fast path; the unique index on
`webhook_events.idempotency_key` is the actual guarantee. Flush Redis and the system stays
correct, just slower.

**A claim is released when the work behind it fails.** Claim the key, then fail to write to
Postgres, and without the release the sender's retry is answered "already handled" for the
next 24 hours — the event is gone and every log line says it succeeded.

The write is one transaction: the raw payload, the dispute, its first audit event and the
outbox message all commit together or none of them do. The relay then drains `outbox` with
`FOR UPDATE SKIP LOCKED`, publishing *before* marking rows published — at-least-once,
because the alternative loses messages on the same crash.

Per-merchant rate limiting is a Lua script rather than GET/compute/SET, since two requests
arriving together would otherwise read the same token count and both spend it. The bucket
is keyed per merchant on purpose: one shared bucket turns a single noisy sender into an
outage for everybody.

### Building it

Go is not installed on this machine yet. Once `go version` works:

```bash
make tidy          # resolves the module graph, writes go.sum
make ingest-test   # signing + validation tests, including a cross-language HMAC vector
make ingest        # listens on :8080
```

Then, in another shell:

```bash
make emit          # the simulator streams signed webhooks at it
```

`npm run emit -- --replay --count 3` in `services/simulator` sends every event twice. The
first delivery answers `202 accepted`, the second `200 duplicate` — that is the Redis
`SETNX` earning its place.

`services/ingest/internal/signing/signing_test.go` pins the exact bytes both languages
hash. If it fails, the Go and TypeScript halves of the contract have drifted apart.

## Next: Phase 2

The Angular dashboard: server-side pagination over 500,000 rows, faceted filters, CSV
export, and a live feed of what the ingest service is accepting.
