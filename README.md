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

**Phase 0 is complete**: schema, ledger and data generator. The rest is the roadmap.

| Phase | What | State |
| --- | --- | --- |
| 0 | Postgres schema, double-entry ledger, Node simulator | done |
| 1 | Go ingest service: HMAC verification, Redis idempotency, outbox | next |
| 2 | Angular dashboard over the seeded data | |
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

## Next: Phase 1

The Go ingest service, at `services/ingest/`:

- `POST /webhooks/processor` — verify the HMAC signature (constant-time; the simulator's
  `signing.ts` is the other half of the contract), reject anything outside the replay window
- `SETNX` the idempotency key in Redis, with the `webhook_events` unique index as the
  durable backstop when Redis has been flushed
- Write the raw payload to `webhook_events` and the parsed dispute plus its outbox message
  in one transaction
- A relay drains `outbox` into the queue

Go is not installed on this machine yet: `brew install go`.
