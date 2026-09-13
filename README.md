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
| **Redis** | Idempotency, distributed locks, per-merchant rate limiting, a sorted set used as a deadline timer, and a pub/sub channel for the live feed. Five distinct patterns, none of them caching. |
| **Angular** | An operations dashboard over half a million rows: server-side pagination, filters, exports, charts, a live feed. |
| **AWS** | SQS for the queue, S3 for evidence uploads, ECS for the services. |
| **Node/TypeScript** | The fake payment processor that generates the world. |

## Status

**All five phases are built**, and the whole platform runs on a laptop with no AWS account:
a signed webhook becomes a row, the relay puts it on SQS, a worker picks it up and decides
it before its deadline passes, and the dashboard shows it happen.

| Phase | What | State |
| --- | --- | --- |
| 0 | Postgres schema, double-entry ledger, Node simulator | done |
| 1 | Go ingest service: HMAC verification, Redis idempotency, outbox | done |
| 2 | Read API and the Angular dashboard | done |
| 3 | Redis deadline timers, Go worker pool, dispute state machine | done |
| 4 | SQS, S3 evidence uploads, on LocalStack | done |

Phase 4 comes last on purpose. Building the queue by hand first and *then* migrating it to
SQS teaches more than reaching for SQS on day one.

## Running it

Requires Docker and Node 20.11+ (the package pins 22 via Volta; `import.meta` path
resolution and `fetch` both need a recent runtime). Nothing else — Postgres and Redis run in
containers, so there is no local `psql` or `redis-cli` dependency.

```bash
cp .env.example .env
make up                    # postgres :5433, redis :6379, localstack :4566 (and provisions AWS)
cd services/simulator && npm install && cd -
cd apps/dashboard && npm install && cd -
make migrate
make seed                  # ~500k transactions; SEED_TRANSACTIONS=20000 make seed for a fast one
make verify                # asserts the money invariants, prints a summary
```

Then three processes, one per terminal (`make stack` prints this):

```bash
make ingest   # :8080  receives signed webhooks
make api      # :8081  serves the dashboard
make worker   #        consumes SQS and decides disputes before their deadlines
make dash     # :4200  the dashboard itself
make emit     # sends disputes at :8080, and they appear on :4200 live
make rule     # the network rules on represented disputes (won/lost)
```

`make rule` closes the lifecycle. Run the worker first — it is what moves evidence-led
chargebacks to `represented`, which is the only state a ruling can act on.

`make aws-status` shows the queue depths and what is in the evidence bucket. `make aws-dlq`
prints whatever ended up dead-lettered.

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

### Chargebacks hold funds on arrival

A chargeback is a clawback that has already happened — the acquirer takes the money when the
network files it, and the outcome lands weeks later. So the entry is posted on arrival, not
at the end:

| Event | Entry |
| --- | --- |
| Chargeback filed | debit `merchant_balance`, credit `disputes_payable` |
| Representment won | debit `disputes_payable`, credit `merchant_balance` |
| Representment lost, or window closed | debit `disputes_payable` + `merchant_balance` (fee), credit `settlement_clearing` + `fee_revenue` |
| Alert refunded | debit `merchant_balance`, credit `settlement_clearing` |

An alert moves nothing until it is refunded, because it is a warning rather than a clawback
— which is exactly why refunding one is the cheap outcome.

The loss is four legs, not a netted two. Both balance; only one of them still contains the
fee, which is the number a merchant eventually asks about.

Two invariants tie this back to the domain tables: `disputes_payable` must equal the
disputed amount of exactly those chargebacks still awaiting an outcome, and no resolved
chargeback may still be holding funds. Both describe errors that balance perfectly and are
still wrong, so nothing else would catch them.

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

## `apps/dashboard/` — the Angular app

Angular 21, standalone and zoneless, signals throughout.

**`httpResource` takes a reactive URL.** It is built from the filter signals, so changing a
filter reissues the request and aborts the one in flight. No subscribe, no unsubscribe, and
no chance of a slow earlier response landing after a faster later one and painting stale
rows over fresh ones. RxJS appears exactly once, to debounce the search box — the one thing
signals have no notion of, since they don't model time.

**Filters live in the URL.** An operator who finds something worth escalating sends a link
and the recipient sees the same rows. `replaceUrl`, so filtering isn't twenty back presses
to leave the page.

**Money is minor units until the moment it renders.** One division, at the formatter, on a
value already rounded by the ledger. The digits-per-currency map is why ¥5,000 renders as
¥5,000 and not ¥50 — a blanket divide-by-100 is wrong by a factor of a hundred and doesn't
look wrong.

**Live arrivals are announced, not injected.** A "3 new — refresh" pill rather than rows
appearing under someone mid-read.

### The two-stage page query

The first version of `/api/disputes` was one flat statement, and `EXPLAIN` showed why it
was slow: Postgres joined all 13,119 matching disputes to their transactions and *then*
sorted and discarded all but fifty. The count query also joined a 500,000-row table whose
columns nobody read.

Now the inner query finds the fifty ids and only those are joined out for display; the
`transactions` join appears only when there is actually a search to run against it.
**35ms → 3-7ms** warm. The work should follow the `LIMIT`, not precede it.

Deep paging is capped at offset 10,000 rather than left to rot — `OFFSET` makes Postgres
walk and discard every row before the window. Past that the CSV export is offered, which
streams row by row and holds 13,000 disputes in 90ms and flat memory.

## `internal/worker/` — the deadline worker

The process that makes this a platform rather than a filing cabinet.

**The queue is a Redis sorted set** scored by deadline. A Lua script pops what is due and
removes it in one operation — done as `ZRANGEBYSCORE` then `ZREM` from Go, two workers
polling at once both read the same ids in the gap between the calls.

**The index is derived, not a record.** A reconcile pass rebuilds it from the open disputes
in Postgres, so flushing Redis costs a pass and nothing else. It also closes the gap the
fast path cannot: a dispute written while Redis was unreachable was never scheduled, and
without the pass it would sit unnoticed until its window closed.

**The policy is a pure function** (`rules.go`), because what the system does with someone's
money is the part that most needs to be readable and testable without standing up a
database:

| Situation | Decision |
| --- | --- |
| Past its deadline, still open | **expire** — a failure written down, not an outcome chosen |
| Alert on a charge already refunded in full | **close** as refunded — the answer is "already refunded", and no money moves |
| Alert, within the merchant's ceiling, room left on the charge | **refund** |
| Alert, above the ceiling or over the refundable remainder | **escalate** |
| Chargeback, any reason code | **escalate** — the assistant drafts, a person submits |
| Draft awaiting a reviewer, deadline passed | **expire** — the same failure, written down the same way |

It never concedes a chargeback, and it never represents one. Auto-refunding an alert is
strictly cheaper than letting it lapse, so it is safe to automate; writing off money is a
judgement about evidence and a merchant relationship, and a rule engine that quietly does
it is the one nobody notices is wrong. Representing means submitting a letter, and the
worker has none: the only path to `represented` is a person approving a draft in the
review queue. (It used to move evidence-led chargebacks there on its own, with nothing
submitted; the review found it.)

**Safety is layered, and the lock is the weakest layer.** Any lock with a timeout can be
held by two processes at once — the holder pauses for a GC, the TTL lapses, and a second
worker acquires it perfectly legitimately. What actually makes a double refund impossible
is the optimistic version check on the dispute row and the unique `external_ref` on the
ledger entry. The Redis lock only means the second worker usually does not bother trying.
It does still release safely, comparing its token before deleting, because a bare `DEL`
deletes whatever lock is there — including somebody else's.

Over the 1,667 open disputes in a fresh seed: **395 refunds, 1,272 escalations, no
errors** — and all 11 money invariants still pass afterwards. (Before the worker stopped
representing, 412 of those escalations were representments with no letter.)

### The livelock

The first version did nothing at all, logged nothing, and pinned a core. `SIGQUIT` showed
every goroutine parked in `SETNX`, but Redis was healthy with four clients — so `MONITOR`
was the tool that answered it, showing the same four dispute ids being claimed,
rescheduled and reclaimed a few thousand times a second.

Claiming takes everything due before `now + lookahead`. Escalation rescheduled at the
dispute's deadline, which is *inside* that window, so it was instantly claimable again.
And because every batch came back full, `drainOnce`'s inner loop never ended, the poll loop
never returned to its select, and the other 1,663 disputes were never looked at.

Two fixes, both invariants rather than patches: every reschedule goes through `nextVisit`,
which guarantees a time strictly outside the claim window, and one poll tick drains at most
a bounded number of batches so the loop always gets back to its select. The heartbeat log
line exists because of this too — a worker that only logs when it acts is indistinguishable
from a worker that is stuck.

## Phase 4 — AWS, without an AWS account

LocalStack runs the real AWS APIs in a container. Same SDK, same calls, same error types,
no account and no spend. Everything below is the actual AWS API being exercised.

**SQS replaced the log stand-in behind the same `Publisher` interface, and the relay did not
change one line.** That is the return on building the outbox pattern first: the queue
underneath it was always a swappable detail rather than an architecture.

**The worker gained an SQS consumer** that schedules a dispute the moment it arrives instead
of waiting up to a minute for the next reconcile pass. The reconcile pass stays — this is
the fast path, that is the guarantee. Deleting a message is the acknowledgement and happens
only after the work is durably done, so a crash means redelivery. That is safe here because
scheduling is idempotent: `ZADD` on an id already present moves it rather than duplicating
it.

**A redrive policy closes the retry-forever gap** left at the end of Phase 3. A message that
fails five times goes to a dead-letter queue instead of being redelivered until the heat
death of the universe — and it is never deleted just to keep the logs quiet, because a lost
message is worse than a noisy one. There is a test that sends a genuinely unusable message
and waits for it to appear in the DLQ.

**Evidence uploads are presigned S3 policies.** The browser posts a form straight to S3, so
a large scan is never that many bytes through a Go process, and the API never has to think
about request body limits. Three details that matter more than the plumbing:

- **The size limit is in the signed policy, not in a field the client is asked to respect.**
  A presigned PUT signs the method, the key and the content type and nothing about the body,
  so its documented maximum was a request. A presigned POST signs a policy document with a
  `content-length-range`, and S3 counts the bytes it actually receives. A 26MB file comes
  back `400 Your proposed upload exceeds the maximum allowed size`, an empty one `400 smaller
  than the minimum`, and editing the signed key afterwards is a `403` rather than an object
  in somebody else's prefix.

- A caller-supplied filename is reduced to something that cannot escape its prefix before it
  becomes a key. `../../delivery proof #7.pdf` lands as
  `disputes/3/1788630168-delivery-proof--7.pdf`.
- Content types are a whitelist, not a blacklist. Evidence is a document or an image;
  `text/html` and `image/svg+xml` are not on the list, because a bucket serving attacker-
  controlled markup is a stored XSS with a CDN in front of it.

### What LocalStack cannot teach you

Be clear-eyed. **ECS is Pro-only**, so nothing here is actually deployed. IAM is not
enforced, so none of this practises least privilege. There is no throttling, no quota, no
cost, and no CloudWatch alarm firing at 3am. The knowledge that transfers is SQS semantics,
idempotent consumers and the presigned-upload pattern — not `aws ecs update-service`.

Pointing all of it at real AWS is clearing `AWS_ENDPOINT_URL` and letting the SDK find
credentials the normal way.

### The upload is the one request that skips HttpClient

The dashboard's detail panel takes a file by picker or drag-and-drop, asks the API for a
presigned URL, and PUTs the bytes straight to S3. Two things worth knowing:

**The app runs `withFetch()`, and the Fetch API cannot report upload progress.** It reports
download progress and nothing else. For a 40MB scan of a delivery receipt that is the
difference between a progress bar and a frozen dialog, so that one call drops to
`XMLHttpRequest`, which has had `upload.onprogress` since 2006.

Two things S3 does not forgive, both found the hard way: every condition in the policy needs
a matching form field — the SDK shapes the policy from `PutObjectInput.ContentType` but does
not add that field, so a valid upload fails with `Policy Condition failed` — and the file
part must come last, because S3 stops reading fields when it reaches it.

**The API's CORS middleware advertised only `GET`.** The presign endpoint is a `POST`, so
the browser would have refused it at the preflight — the server never even sees a blocked
request, which is what makes this class of bug quiet. There are preflight tests now,
including one asserting no origin ever receives a wildcard.

### Draining the dead-letter queue

A dead-letter queue nobody can read is a bin. The point of one is the loop:

```bash
make dlq              # what failed, how old, how many times it has been retried
make dlq-replay       # dry run
go run ./cmd/dlq replay
```

Replay sends before it deletes — a crash between the two redelivers a message, which the
consumer is built for, where deleting first loses it. Each replayed message carries a
`redrive_count`, and one already put back three times is refused: SQS's own receive count
resets on re-send, so without carrying this a message loops forever and looks new every
time.

Two bugs found building it, both worth keeping in mind. **Approximate counts must never gate
behaviour** — `replay` run straight after `peek` reported an empty queue and did nothing.
And **a dry run has to be a no-op in every observable sense**: the first one reserved the
messages for thirty seconds, so its report was accurate and its effect was a lie.

The cause of the second is a genuine SDK trap. `ReceiveMessage`'s `VisibilityTimeout` is
serialised only when non-zero:

```go
if v.VisibilityTimeout != 0 { s.WriteInt32(...) }
```

so passing `0` is indistinguishable from omitting it, and the queue default applies.
`ChangeMessageVisibility` writes it unconditionally, which is the only way to actually hand
a message straight back.

### Signing keys, and rotating them

`merchants.webhook_secret` was readable by anything holding a database connection: every
service, every migration, every analyst with production read access, every backup of that
table. The keys now live in one Secrets Manager document behind a single IAM permission,
cached with a TTL — and that TTL *is* the rotation latency, so it wants to be minutes.

**Verification accepts a set of keys, not one.** Rotation is not an instant: the new key is
published, senders pick it up over minutes, and deliveries signed with the old one keep
arriving throughout. Accepting a single key forces a cutover that rejects every in-flight
delivery, which is why keys that can only be rotated with an outage never get rotated.

`Merchant` no longer carries the key at all — it used to, so every path that wanted a
merchant's currency also held its credential, and any log line dumping the struct leaked it.

The database source stays as a fallback, because a secret store you cannot fall back from is
a single point of failure wearing a security badge.

## `infra/terraform/` — the deployment, as a design

ECS task definitions, IAM policies, queues, bucket, alarms. **Never applied** —
there is no AWS account behind this project — so it is a design to read and
critique, not a deployment to trust. `infra/terraform/README.md` is the longer
piece: what Terraform is, why choose it over CloudFormation or a shell script,
what state actually is, and the honest arguments against it.

The two things worth taking from it:

**An ECS task has two IAM roles and they are used by different things.** The
*execution role* is assumed by the ECS agent before the container starts — it
pulls the image, creates the log stream, resolves injected secrets. The *task
role* is assumed by the process at runtime and authorises every SQS and S3 call
the code makes. If a task never starts, suspect the execution role; if it starts
and then returns `AccessDenied`, suspect the task role.

**Compare `messaging.tf` with `infra/localstack-init.sh`.** They create the same
queue. The script says *how* and cannot tell you what it is about to change,
cannot notice that somebody widened a timeout in the console, and cannot delete
what it made. Terraform says *what*, and `plan` prints the difference before
anything happens. The cost is a state file, which holds secrets in plaintext and
is a genuine liability — worth naming rather than glossing over.

## `cmd/mcp/` — the read model over MCP

An MCP server exposing the dispute queue to an assistant, so it can look instead
of guessing. Point a client at `.mcp.json` and ask *"why did dispute 12458
escalate?"* — it reads the state history and the ledger and answers from them.

Four tools: `list_disputes`, `get_dispute`, `get_customer_history`,
`queue_summary`. A thin adapter over the same `internal/api` read model the
Angular dashboard uses.

**The design is what is missing, not what is there.** Every tool is read-only,
and the bottom of `internal/mcpserver/server.go` lists what was deliberately left
out:

- **No tool that writes.** A model decides on its own when to call things,
  prompted partly by text other people wrote. The blast radius of that should not
  include money.
- **No generic `run_query`.** The convenient thing to build and the wrong thing
  to ship: it collapses every access decision into "can it write SQL".
- **No presigned evidence URLs.** A presigned URL is a bearer credential with a
  TTL, and handing one to a model puts it in a transcript that gets logged and
  pasted into tickets. Where a model needs what a file says - the drafting
  agent does - host code reads the bytes and puts the text on the record.
- **No unmasked customer emails.** `customer_ref` already answers "is this the
  same person"; the address itself never needs to leave the database.

Two smaller decisions worth the same scrutiny. Lists are capped at 50 — a context
limit, not a performance one — and a truncated answer *says so*, because silent
truncation is how an assistant states a wrong total with confidence. And an
overdue dispute carries an explicit `overdue: true` rather than leaving it to the
sign of `hours_to_deadline`: a negative number is easy to skim past, and the
difference is whether the reader thinks there is time left.

Tested with a real client over the SDK's in-memory transport, including one test
that fails if a tool ever stops being read-only.

```bash
make mcp-check
```

### `cmd/ask/` — one question over the same tools

The operator-facing surface the loop in `internal/agent/loop.go` was written
for: a model in a loop over the four read-only tools, asked something in plain
words, allowed to look things up, and stopped by a turn ceiling and a cost
ceiling.

```bash
go run ./cmd/ask -dry-run                                  # the tools and the budget, spending nothing
go run ./cmd/ask "which merchant has the most overdue chargebacks?"
```

It answers only from tool results, repeats the tools' own "truncated" notes
rather than totalling what it saw, and says in words when a ceiling stopped it
before it finished. The drafting flow deliberately does not use this loop - two
judges need one record - so until this command existed the loop was a harness
nothing ran.

## Where the context lives

- **[`docs/DECISIONS.md`](docs/DECISIONS.md)** — why the project is shaped the
  way it is, including the alternatives that were rejected. This README says what
  exists; that says what was chosen against, which is the part that gets lost.
- **[`docs/conceitos-pt.md`](docs/conceitos-pt.md)** — the same ground explained
  in Portuguese, for study rather than record: why Terraform exists and what
  category of thing it is, why Go rather than Node here, what HMAC proves, and
  why these are long-running processes rather than serverless functions.
- **[`.spec/agent-harness/part-b.md`](.spec/agent-harness/part-b.md)** — the
  representment assistant: built, with what was measured. The loop, the
  independent verifier, the guardrails, the evals, and the order it was built in.
- **[`.spec/review-fixes/plan.md`](.spec/review-fixes/plan.md)** — what an
  adversarial review found on 2026-09-10 and the order to fix it in.

`docs/` describes what is; `.spec/` describes what isn't yet. When something in
`.spec/` ships, the decision worth keeping moves to `docs/` and the plan is
deleted — git is the archive.

## Known gaps

- **`POST /api/reviews/{id}/decision` is unauthenticated**, and it is the one
  endpoint that moves a dispute to `represented`. The reviewer is a name in a
  request body, so `reviewed_by` records who a caller *claimed* to be. Fine for
  a local dashboard, and the first thing that has to change before this service
  is exposed: an audit trail is worth what the identity in it is worth.
- The agent runs against the Anthropic API with a key from
  console.anthropic.com. A Bedrock `Completer` existed beside it for a
  deployment where the task role would supply the credential; it never
  executed and was removed rather than kept as untested code.
- Nothing is deployed anywhere; ECS needs a real AWS account. The Terraform is
  `fmt`-clean, `validate`s, and `plan`s to 60 resources under OpenTofu, but has
  never been applied.
- `merchants.webhook_secret` still exists, because it is the `database` fallback
  source. Secrets Manager is the default (`WEBHOOK_SECRET_SOURCE=secretsmanager`)
  and the ingest service reads from it, but the column is still a place a signing
  key can be read from — the fallback that makes the project runnable with no AWS
  is also the exposure the move to Secrets Manager was meant to close.
