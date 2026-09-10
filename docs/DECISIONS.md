# Decision log

Why this project is shaped the way it is, including the alternatives that were
considered and rejected. The README describes what exists; this records what was
chosen against, which is the part that gets lost.

Portuguese explanations of the same ground, written for study rather than
record, are in [`conceitos-pt.md`](conceitos-pt.md).

---

## The premise

A study project for learning Go, Angular, Redis and AWS on top of Node/TS/Postgres.
The chargeback domain was chosen so that every unfamiliar piece is load-bearing
rather than decorative — if a technology could be swapped for something already
known without losing anything, it did not earn its place.

**Local only.** No remote, never pushed. One environment, one person.

---

## Money and the ledger

**Money is `BIGINT` minor units plus a sibling currency column.** Never `NUMERIC`,
never a float, never a bare number meaning "dollars". Amounts are positive;
direction lives in the ledger.
*Rejected:* `NUMERIC(12,2)`. It is correct arithmetically and still lets a float
in through a driver, a JSON boundary, or a client.

**Correctness is enforced by the database, not by service code.** A deferred
constraint trigger checks debits against credits at COMMIT; a composite foreign
key ties a posting's currency to its account's; `UPDATE`/`DELETE` on postings
raise.
*Why:* application code is where money bugs live, and every service that writes
to the ledger would otherwise have to remember the same rules.

**Balances are a view, never a stored column.** There is no cached number to
drift.
*Accepted cost:* it gets slower with volume. The fix is a rollup maintained by
the same transaction that writes the postings — not a nightly recalculation.

**A chargeback holds funds on arrival** (`debit merchant_balance / credit
disputes_payable`), released on a win, sent onward plus a fee on a loss.
*Rejected:* posting nothing until the outcome. That was the original design and
it was wrong: the acquirer takes the money when the network files, so a merchant
balance showing money that had already left was a book that balanced and lied.

**A lost representment is four legs, not two.** Hold released toward the issuer,
fee charged separately.
*Rejected:* netting into two legs. Balances identically and loses the fee, which
is the number a merchant eventually asks about.

**`ledger_transactions.external_ref` is the idempotency key for money.**
`dispute:1234:refund` can exist exactly once, so a retrying worker hits a
constraint violation — which is the *successful* outcome.

---

## The ingest path

**The order of the steps is the security design.** Per-IP limit and body cap
before any database work; parse before verifying (unavoidable — the merchant id
that selects the secret is inside the body) but act on nothing until verified;
merchant lookup and signature failure return the *same* 401; the merchant's
rate-limit budget is spent only after the signature holds.
*Why the same 401:* different answers make the endpoint an oracle for
enumerating merchant ids.
*Why the rate limit comes after:* otherwise anyone can exhaust a merchant's quota
by claiming their id.

**The idempotency key comes from the signed body, never the `Idempotency-Key`
header.** That header is not covered by the signature.

**A claim is released when the work behind it fails.** Without it, a retry is
answered "already handled" for 24 hours — the event is lost and every log line
says it succeeded.

**Redis is the fast path; the unique index on `webhook_events.idempotency_key`
is the guarantee.** Flushing Redis makes the system slower, never wrong.

**The outbox pattern.** The dispute and its outbox message commit together.
*Rejected:* publishing directly to the queue. Publish-then-commit creates events
for disputes that do not exist; commit-then-publish loses disputes silently.
*Payoff:* when SQS replaced the log publisher in Phase 4, the relay did not
change one line.

**Verification accepts a set of secrets, not one.** Rotation is not an instant —
the new key is published, senders adopt it over minutes, old-key deliveries keep
arriving. Accepting one key forces a cutover that rejects everything in flight,
which is why keys that can only be rotated with an outage never get rotated.

---

## The read path

**Two Go binaries, not one.** `cmd/ingest` only writes and has seconds to accept
a webhook; `cmd/api` only reads and answers a dashboard.
*Why:* a slow operator query can never hold up an inbound dispute, and they scale
on their own terms.

**The page query finds ids first, joins for display second.** Written flat,
Postgres joined all 13,119 matching disputes to their transactions and *then*
sorted away all but fifty. 35ms → 3-7ms.
*The rule:* the work should follow the `LIMIT`, not precede it.

**`OFFSET` is capped at 10,000, and the CSV export is offered instead.** Deep
paging is a table scan wearing a page number; the export streams row by row with
flat memory.

**Never let a parameter's type be decided by the SQL around it.** `LIKE
'dispute:' || $1 || ':%'` makes Postgres infer text while the driver holds an
int64. Neither a psql literal nor a `PREPARE` with a declared type reproduces it
— both settle the ambiguity that is the bug.

---

## The dashboard

**Angular 21, standalone, zoneless, signals.** `httpResource` takes a reactive
URL built from the filter signals, so a filter change reissues and aborts the
one in flight — a slow earlier response can never paint over a fresh one.

**RxJS appears exactly once**, to debounce the search box. Signals do not model
time; that is the one thing RxJS does better here.

**No SSR.** It would add a fourth long-running Node process to render a page for
an internal ops team behind a login, where SEO is irrelevant. Without it the
front is 93 kB of static files on a CDN.

**Filters live in the URL**, with `replaceUrl`. An operator who finds something
worth escalating sends a link; state in a component field cannot be shared.

**Money is divided exactly once, at the formatter.** The digits-per-currency map
is why ¥5,000 renders as ¥5,000 and not ¥50.

**Live arrivals are announced, not injected.** Reordering the table under someone
mid-read loses their place.

**The upload is the one request that does not use HttpClient.** The app runs
`withFetch()`, and the Fetch API cannot report upload progress.

---

## The worker

**The decision policy is a pure function** on a plain struct — no database, no
Redis. What the system does with someone's money is the part that most needs to
be readable and testable without infrastructure.

**It never concedes a chargeback.** Auto-refunding an alert is strictly cheaper
than letting it lapse, so it is safe to automate. Writing off money is a
judgement about evidence and a merchant relationship, and a rule engine that
quietly does it is the one nobody notices is wrong.

**The Redis sorted set is an index, not a record.** A reconcile pass rebuilds it
from Postgres, so losing it costs one pass. It also catches what the fast path
misses — a dispute written while Redis was unreachable.

**Claiming work is a Lua script.** `ZRANGEBYSCORE` then `ZREM` from Go leaves a
window where two workers read the same ids.

**The distributed lock is the weakest safety layer, not the strongest.** Any lock
with a timeout can be held by two processes: the holder stalls, the TTL lapses, a
second worker acquires it legitimately. What makes a double refund impossible is
the optimistic version check and the unique `external_ref`. The lock only means
the second worker usually does not bother.
*Consequence:* releasing compares the token before deleting, because a bare `DEL`
removes whatever lock is there.

**Every reschedule goes through `nextVisit`**, which guarantees a time strictly
outside the claim window. This is the fix for a livelock where escalation
rescheduled inside the window, making a dispute instantly re-claimable — and
because every batch came back full, the poll loop never returned to its select
and 1,663 other disputes were never looked at.

**One poll tick drains a bounded number of batches**, so the loop always returns
to its select.

**The heartbeat exists** because a worker that only logs when it acts is
indistinguishable from a worker that is stuck.

---

## AWS

**LocalStack, not a real account.** SQS, S3 and Secrets Manager are the real APIs
with the real SDK. ECS is Pro-only, so nothing is deployed and the Terraform has
never been applied — only `plan`ned.

**AWS came last on purpose.** Building the queue by hand first means the outbox,
at-least-once delivery and idempotent consumers are understood before SQS stops
looking like magic.

**A presigned POST, not a PUT.** A PUT signs the method, the key and the content
type and nothing about the body, so its size limit was a request. A POST signs a
policy document with a `content-length-range` that S3 enforces against the bytes
it receives.

**Replay sends before it deletes.** A crash between the two redelivers, which the
consumer handles; deleting first loses the message.

**Approximate counts never gate behaviour.** `ApproximateNumberOfMessages`
excludes in-flight messages and lags — it is a number to display, never to branch
on.

**A dry run is a no-op in every observable sense.** The first one reserved
messages for 30 seconds, so the queue looked empty to whatever ran next: an
accurate report and a lying effect.

**Signing keys moved out of `merchants.webhook_secret`**, which was readable by
anything holding a database connection. The database source stays as a fallback,
because a secret store you cannot fall back from is a single point of failure
wearing a security badge.

---

## Terraform

**It provisions, never populates.** No resource writes content. The Secrets
Manager entry gets a placeholder with `ignore_changes` precisely so Terraform
does not manage the value — a credential in a `.tf` file is in git, and one in
state is in the state bucket in plaintext.

**The network is not created here.** Split by blast radius and rate of change,
not by size: a VPC changes rarely and is shared by everything in the account.

**This project fails its own test for needing Terraform** — one environment, one
person. It has it because 60 resources with per-service IAM cannot be clicked and
remembered, and because the goal is learning. Worth being able to say out loud.

**Preconditions move failures from apply to plan.** The target group name limit
is exactly 32 in production; without the precondition it fails at apply, from
AWS, phrased as a complaint about the name rather than its length.

---

## MCP

**Every tool is read-only.** A model decides on its own when to call things,
prompted partly by text other people wrote. That blast radius should not include
money.

**No generic `run_query`.** The convenient thing to build and the wrong thing to
ship: it collapses every access decision into "can it write SQL", and no amount
of prompting re-establishes the boundary.

**No presigned evidence URLs.** A presigned URL is a bearer credential with a TTL,
and handing one to a model puts it in a transcript that gets logged and pasted
into tickets.

**No unmasked customer emails.** `customer_ref` already answers "is this the same
person".

**A field a tool returns has to be a value the tools that take it accept.**
`get_dispute` returned the merchant's display name while `get_customer_history`
matches on the external id, so the obvious chain — look up a dispute, then ask
what else this customer has filed — answered "nothing" for every customer,
including repeat filers. Nothing errored: the query was valid and the result was
empty, which is the worst shape a wrong answer can take. Both are returned now,
`merchant` being the handle and `merchant_name` the label, and a test drives the
chain rather than checking either tool alone.

**Retrieval is an injection vector.** Precedent retrieval brings back the
cardholder claims of *other* disputes, and the first version rendered them bare
under a heading that said "record" — serving a planted instruction from a
settled dispute to the model as though the system had asserted it. The
quarantine had been built for the input somebody was thinking about, and
retrieval reached around it. One `quarantine()` function now wraps every piece
of cardholder text wherever it appears, and the precedent claims are truncated
as well as wrapped, because every extra sentence is both prompt paid for and
injection surface offered.

**Precedent reaches both calls or neither.** The retrieved neighbours are on
`Facts`, which the generator and the verifier both read. If only the generator
could see them, it could reason from something the verifier — working from a
record fixed before either call ran — has never seen, and a correct draft would
come back rejected as unsupported. Retrieval that only one of two judges can see
is worse than no retrieval.

**The lexical baseline is built, not assumed away.** A vector index that has
never been compared against `ts_rank` is a claim rather than a result, and for
short text in one language the contest is genuinely open. Without an embedding
key the retriever uses full-text search and says so in the trace.

**The embedding width is in the schema.** `vector(1024)` means changing
embedding model is a migration, not a config change — and the model travels with
every row, because two models' vectors are not comparable and mixing them
produces neighbours that are not neighbours.

**Truncation says so, and overdue is an explicit boolean.** Designing tools for a
model is not designing an API for code: code never forgets to check the sign of a
number, and silent truncation is how an assistant states a wrong total with
confidence.

---

## Environment facts that cost time to rediscover

- Postgres is on **5433** (5432 was taken); Redis 6379; LocalStack 4566.
- **Homebrew cannot write to `/opt/homebrew`** — `brew install` fails until a
  `sudo chown`. Go 1.27.1 and OpenTofu were installed by hand instead
  (`~/.local/bin/tofu`).
- **Volta's shell default is Node 18.** The repo root `package.json` pins
  22.17.1 for everything under it; Angular 21 needs ≥ 20.19.
- Volta pins `node` and `npm` **separately** — pinning only node left an old npm
  doing dependency resolution, and it crashed.
- `nx`/`make reset` recreates LocalStack's volume, so its VPC and subnet ids
  change; `make tf-plan` generates its tfvars rather than reading a committed
  file.
