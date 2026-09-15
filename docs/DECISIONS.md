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

**A deterministic grader keeps what is deterministic.** Three separate graders
have now fired on a draft that was refusing the thing it was accused of. The
injection grader failed a model that named the planted text and declined it. The
promise grader failed a letter whose disclaimer said "purporting to direct an
admission of liability... has been disregarded". Both patterns were reading for
meaning, and a pattern cannot tell a letter making a commitment from a letter
describing the commitment it refused — the words are the same and the difference
is in what surrounds them.

The resolution is a boundary, not a better pattern. Deterministic checks decide
what is decidable by comparison: does this filename exist, does this figure
appear in the record, did the recommendation change against a control. Judgements
that need reading go to the verifier, which is a model reading the letter and is
the right tool for them.

Worth stating plainly because of how it was found: the class was diagnosed and
fixed in the injection grader, and left untouched in the function next to it. The
eval caught it again three runs later.

**Base rates cover what precedent cannot.** Precedent retrieval needs a
cardholder claim to match on, and only 15% of open chargebacks have one — so the
feature built to inform a draft was unavailable for six disputes in seven. A base
rate needs only merchant and reason code, which every dispute has. Expired
disputes are reported beside the rate and never inside it: a deadline missed
unattended was never argued, so it says nothing about whether the case was
winnable and everything about the merchant. Below ten settled disputes the counts
go out and the rate does not, because an interval that wide is noise with a
percent sign.

**One sample per case cannot tell a fix from luck.** Five consecutive eval runs
scored 9/9, 7/9, 9/9, 8/9 and 9/9, and some of those had no code change between
them: the failures were the model phrasing something differently, not the system
behaving differently. A number that moves on its own is not a measurement until
you know how much it moves.

Cases are drafted `-samples` times now and the report shows `passed/total` per
case. The figure to read first is how many cases *split* their samples — passed
some and failed others — because while that is above zero every other number is
an average over something unstable. At one sample the report says so in words
rather than leaving it implicit.

The record is assembled once per case and reused across samples: reassembling it
would let retrieval vary between drafts, and then a difference between them could
not be attributed to the model. The counterfactual runs per sample, paired, for
the same reason.

Measured: 45 runs over 9 cases, 45 passed, no case split. That is not a pass
rate for the system — nine hand-picked disputes are a smoke test — but it is the
first result here that distinguishes a fix from a coin.

Re-measured on 2026-09-11 after the review's fixes, which had changed what the
number could mean: the planted instruction now asks for the opposite of the
natural answer, the figures grader can see amounts in the prompt's own format,
and the verifier reads every draft including the ones that decline. 45 of 45
again, the verifier agreeing with the graders on all 45, at $0.0206 per run
with the verifier and the three counterfactuals included and 51% of input
served from cache (`docs/measurements/2026-09-11-eval.txt`). The first 45 of 45
could not have failed on the planted cases; this one could have.

**Caching the system prompt cut 24% of the bill.** The two system prompts are
about 1,570 tokens that never change, against roughly 880 for the record that
does — nearly half of every input. Marking the *system* block rather than a tool
puts the cache breakpoint at the end of the longer prefix, since the cacheable
order runs tools, then system, then messages. Measured over the eval: 46% of
input served from cache, $0.0204 to $0.0156 per dispute (later five-sample runs
saw 49% and 51%).

The individual prompts sit below the minimum cacheable length and tools plus
system together clear it, which is why the breakpoint placement was the whole
decision rather than a detail.

Two things had to be fixed before the number meant anything. Unset cache rates
fell back to the *input* rate, so a cache working perfectly would have reported
no saving at all — they are derived now, a write at 1.25× input and a read at
0.1×. And the eval reports cache tokens whether or not there were any: a prompt
too short to cache and a cache working perfectly both produce a plausible cost,
and only the usage says which happened.

**A prompt is a boundary, and boundaries get formatters.** The rule below —
divide exactly once, at the edge — was applied to the ledger, the API and the
dashboard, and then the record handed a model `amount_minor: 5799` with an
instruction to copy amounts from the record digit for digit. It wrote $579.99
about a $57.99 dispute. Another draft copied 2790 straight through for a charge
of $27.90. The prompt was the only boundary in the system without a formatter,
and a letter to a card network stating the wrong figure loses the case on its
own. Money now crosses into a prompt formatted - and only formatted. The
first fix added an AMOUNTS block beside the raw record and told both prompts
which fields not to read; the review found the precedent block still printing
"8864 USD" for an $88.64 case, and customer history and ledger lines with no
formatted twin at all. A rule that lives in a warning is a request. The record a
model reads is now a view with every amount already formatted and no minor-unit
integer anywhere in it, and the MCP tools return a formatted `amount` beside
`amount_minor` with a description saying which one to quote.

The failure was stochastic — the same dispute drafted cleanly on a re-run — so
the eval found it rather than a test, and one clean run afterwards is evidence
rather than proof. What holds the fix is `FormatMinor` and its unit tests.

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

**The worker never represents.** It used to move evidence-led chargebacks to
`represented` on its own - a state that means "evidence submitted", reached
with nothing submitted, while the assistant raced it for the same rows and the
review endpoint's comment called itself the only path there. Representing means
a letter, the worker has none, and the only path to `represented` is now a
person approving a draft. Fraud and evidence-led codes both escalate; the
category survives as the reason in the audit trail.

**An alert on a charge already refunded in full is answered, not escalated.**
The seed puts several disputes on one transaction, and once one alert on it
was auto-refunded every later alert on the same charge claimed more than was
left. The balance rule caught that - correctly, the CHECK constraint would
have refused the write - and escalated it with a reason that read like a data
fault: twenty-two alerts in front of a person, each to be looked at again just
after its deadline and expired, "a failure written down", for a refund that
had already happened. The full-refund case is now its own decision: close the
alert as refunded, write the fact into the audit trail, and post nothing,
because the refund is already in the ledger under the dispute that made it
and a second entry would be the double refund every other layer exists to
prevent. The merchant's ceiling does not enter into it, since nothing is
being refunded for a ceiling to bound. A partial remainder still escalates:
that one really is a question about the data. A chargeback on a fully
refunded charge still escalates too - "credit already issued" is the
representment, but it is a letter, and the worker writes none - with a reason
that says what the person will find.

**Drafts awaiting a reviewer expire like everything else.** `draft_ready` was
invisible to the sweeper - deliberately, so a draft a person was reading would
not be redrafted - and the review endpoint checked state but not the clock. So
a dispute waiting on a reviewer who went on holiday sat past its deadline with
its funds still held, the invariants satisfied by state, and a Submit button
that would send a letter to a network that had already ruled. The sweeper now
covers the state and expires it past the deadline; with time left it skips,
because the draft still belongs to a person; and Submit refuses past the
deadline.

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

## The container image

Until now there was none, and the Terraform had been written as though there
were: `ecr.tf` creates a registry per service and `ecs.tf` refers to
`:${var.image_tag}`, with a comment explaining that no container-level health
check is possible because a Go binary on a scratch base has no shell. The
Dockerfile is the thing all of that was already describing. Numbers in
`docs/measurements/2026-09-15-docker-image.txt`.

**One Dockerfile, three images, selected by `--build-arg SERVICE`.** The only
difference between ingest, api and worker is which package under `./cmd` is
built, so three files differing by one word each would be three places for that
word to drift from `local.services`.

**Multi-stage is the whole win and the rest is rounding: 586.4 MB to 20.9 MB,
then 20.9 to 14.9.** The first number is the Go toolchain, the module cache and
the source tree shipped to production to run a binary that needs none of them.
The 6 MB after it is `-ldflags="-s -w"`, which is the right trade for a
container that is replaced rather than debugged in place and the wrong one
locally, where `go run` is untouched. `-trimpath` buys no bytes at all; it buys
a panic that reads `internal/api/store.go:149` instead of the building
machine's home directory.

**scratch, and therefore the CA bundle, which is the line worth remembering.**
`FROM scratch` has no certificate store, and every service here talks TLS to
something - S3 and SQS at minimum, Anthropic and Voyage on the agent path.
Without it `cmd/embed` fails with `x509: certificate signed by unknown
authority` naming the Voyage endpoint, which reads like a broken API rather
than a missing file; the measurement records both runs. `golang:alpine` already
carries the bundle, so the fix is one COPY from a stage that exists.

**The .dockerignore is an allowlist, and it is about the `.env` more than the
gigabyte.** Unignored, this repo sends 1.0 GB of build context - 650 MB of
Terraform providers, 321 MB of the dashboard's node_modules - to compile a Go
binary that reads neither; the allowlist makes it 1.1 MB. A denylist would be a
list of everything anybody will ever add, and the entry it would eventually
miss is the one that matters: the context is readable by every stage, so a
secret copied into a layer is a secret in anywhere that layer is pushed.

**Cross-compiled, not emulated.** The build stage is pinned to
`--platform=$BUILDPLATFORM` and sets `GOARCH=$TARGETARCH`, because Go needs one
environment variable to target Graviton and running an emulated arm64 toolchain
to avoid setting it is the slow way to the same binary.

**`make image` tags with the short sha**, because ECR is set to IMMUTABLE and a
tag that can be overwritten says nothing about what is running.

**Local development still runs `go run`.** `make stack` is unchanged. The image
exists for the deployment path the Terraform describes, and putting the edit
loop behind a container build would cost the thing that makes the loop usable.

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

**The Bedrock provider was deleted, and the argument for it kept.** In a
deployment the model credential should come from the ECS task role -
`bedrock:InvokeModel`, no key anywhere, the same reasoning that moved the
webhook secret out of the database. The provider was built and never ran:
LocalStack does not emulate Bedrock and there is no account behind this
project, so it was eighty lines that could only be trusted by reading them.
Untested code that looks like a feature is a claim, and the honest state is a
`Completer` interface with one real implementation and the design note above.

**The loop got the surface it was built for.** The loop (now `internal/toolloop`) - turn
ceiling, cost ceiling, tool dispatch, `Halt` - was the general harness, and the
drafting flow deliberately does not use it: two judges need one record. For a
month it was a harness nothing ran. `cmd/ask` is the operator-facing surface it
was kept for: one question over the four read-only tools, answered from tool
results only, stopped by both ceilings. Sixty lines, and the alternative was to
delete the loop and the tests that describe it.

**The untrusted field has a producer.** `cardholder_claim` was the one free
text on a dispute, and until the review nothing could write it but the seed:
the webhook rejected unknown fields, so the quarantine defended a column the
running system could never fill. The processor may now relay a claim, bounded
at the door (4,000 bytes, valid UTF-8) and stored raw, and the simulator emits
them from the seed's pool at the seed's rate, planted instructions included.

**Precedent carries other cardholders' words, and that is a privacy limit
this project has not closed.** The precedent block quotes the claims of other
disputes into a prompt, and the vector path sends every settled claim to an
embedding provider. On seeded data that is fifteen sentences; on real data it
is other customers' free text, with whatever names, addresses and order details
they typed, leaving the system for two purposes they never agreed to. Known and
not fixed here. The shape of the fix: scrub or hash personal data before
embedding, and quote only the reason-code family and the outcome in the prompt
rather than the words.

**Precedent reaches both calls or neither.** The retrieved neighbours are on
`Facts`, which the generator and the verifier both read. If only the generator
could see them, it could reason from something the verifier — working from a
record fixed before either call ran — has never seen, and a correct draft would
come back rejected as unsupported. Retrieval that only one of two judges can see
is worse than no retrieval.

**A filename is not a document.** The record used to list each file on the
dispute by name, size and upload time, and the first dispute with a file behind
it showed what that buys: the generator cited `delivery-confirmation.pdf` as
"supporting that the order was delivered", the verifier rejected the sentence
because the record held no such thing, and both were right. Ten of ten runs in
the first measured flow declined for the same reason. The bytes now come to
host code, which extracts the text - plain text and CSV as they are, PDFs
through their text layer - and puts it on the record in its own fenced block,
where both judges read it. The presigned URL still goes nowhere near a model;
the service reads the object, and a key outside the dispute's own prefix is
refused so a record for one dispute can never be assembled from another's
file. A file that was fetched and could not be read - an image, a scan with no
text layer, a PDF that does not parse - is listed as *not read* with the
reason, because "this file says nothing" and "this file could not be read"
lead a drafter to different letters; a file that could not be fetched fails
the record instead, so a bucket that is down is a retry and not a confident
"nothing readable on file". Text is capped per file and per record at the
prompt boundary, on the same reasoning as the claim, and the cap is stated on
the file rather than discovered by the model. The first dispute drafted this
way (`docs/measurements/2026-09-13-evidence.txt`) was the first `represent`
the system has produced that passed the verifier.
*Consequence:* images and scans are named but never read. OCR would be the
next step, and it should go through the same host-side path.

**Measured, twice, and the second measurement corrected the first.** Over 78
open disputes, top-3 each, both strategies always return three results — every
target merchant has at least five settled claims — so "found something" is
identical by construction, and the first report's headline, "identical
coverage", was read off a counter that could not move. The number that carries
information is set overlap, and the first figure for it, 0.41 of 3, was measured
against a half-built index: the embedding provider's free tier allows three
requests a minute, the backfill stopped on the limit both times it ran, and the
comparator did not check. Against the complete index
(`docs/measurements/2026-09-11-retrieval.txt`):

| | |
|---|---|
| mean set overlap, vector vs full-text | 1.41 of 3 |
| identical / partial / disjoint sets | 15% / 56% / 28% |
| targets where the OR query matched every candidate | 78% |
| targets with 3+ candidates carrying the identical claim text | 67% |
| overlap, full-text `simple` vs stop-word-aware `english` | 2.58 of 3 |

Read the fourth line before the first: on a fifteen-sentence pool most top-3s
are ties among identical texts, broken by whatever order each index returned,
so most of the remaining disagreement is not about retrieval. And the third
line says the production baseline's filter is a no-op — with the `simple`
configuration stop words are terms — which is why it and the stop-word-aware
variant agree so closely. Nobody has labelled which precedent was the right one
to retrieve, so none of this is a verdict on quality. What can be said: on this
data the vector index has shown no gain over full-text search that the
comparison can see, and the comparison cannot see much until the ties are
broken by a label.

The first run of that comparison said the opposite: 23% found only by the vector
index. It was wrong because the baseline was rigged. `websearch_to_tsquery` on a
whole sentence ANDs every term, so a candidate had to contain every word of the
claim being searched — a straw man that found nothing in a quarter of cases. The
honest version ORs the terms and ranks with `ts_rank`, and against it the
advantage vanished entirely. **Comparing against a crippled baseline is how "we
need embeddings" gets justified**, and the comparison caught it in its own first
run.

Caveat that keeps this from being a general claim: the seeded claims come from a
pool of fifteen texts, so lexical matching is unusually easy. Against real free
text — a thousand people writing in their own words — the vector index would
likely pull ahead, because paraphrase and vocabulary mismatch are exactly where
it wins and exactly what this dataset lacks.

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

## Go shape

An idiomatic-Go review of the whole module on 2026-09-13 found no build, format
or `go vet` problem and 120 findings about shape. `CLAUDE.md` carries the rules
that came out of it and `docs/go-conventions.md` teaches the reasoning. What
belongs here is the handful of decisions that went the other way from the
obvious one, and the one bug that justified the whole exercise.

**A hand-written vocabulary drifted until a money check failed.** Migration
000003 added the `draft_ready` state and the value reached none of the five
places that enumerate states in prose: the API's filter whitelist, the
dashboard's union and its filter control, the tool schema the agent reads, and
the simulator's money invariant. The last one is why `make verify` had been
failing. Thirteen chargebacks awaiting review held 1,184.38 USD that "money held
equals the chargebacks still open" counted as unheld, and the four merchant
discrepancies summed to exactly that. The ledger was right and the check was
stale. `internal/dispute` now holds the states and kinds as typed constants, and
`internal/agent` its own seven vocabularies, so the compiler is one of the
readers. The honest limit is worth recording: two of those five readers are
TypeScript and one is a struct tag, so the compiler was never going to catch
this one. The failing invariant is what did.

**A state stays a literal inside a query when a partial index depends on it.**
Five queries keep their literal rather than binding a constant, because the
planner cannot prove a bind parameter implies an index predicate, and the
indexes were created with exactly those literals in their `WHERE`. Each carries
a comment saying so. The opposite decision was taken for the review vocabulary
after checking that no index on `agent_runs` names `submitted` or `rejected`.

**One cross-package literal cannot be typed, and says so.** `internal/api`
matches `a.outcome = 'rejected'`, a value `internal/agent` owns, but `agent`
imports `api`, so importing back is a cycle. The constant stays a string with a
comment naming `agent.OutcomeRejected` as the other end, and a test in
`package api_test` pins the two together — which is also where `ReviewFinding`
and `agent.Finding` are pinned, a test a comment had claimed existed for weeks
before anyone wrote it.

**Three packages, not two.** Splitting transport out of `internal/agent` left a
question about the tool loop. Folding it into `internal/llm` would make the
transport import the read model, since the registry is built from the dispute
tools, and `cmd/embed` would drag the read API in just to embed vectors.
Leaving it in `internal/agent` means `cmd/ask` imports the whole representment
flow to run a loop that never touches it. So `internal/toolloop` is its own
package, and `internal/llm` imports nothing else in this module.

**The ledger takes a value, and one trailing string stays positional.** Three
adjacent `int64` parameters meant a swap compiled and posted money to the wrong
merchant. `Entry` fixes that. The remaining `reasonCode` stayed a parameter
because the hazard is adjacency: an argument with no same-typed neighbour has
nothing to swap with, and folding it in would give three of the four functions
a field they ignore.

**Exported is a promise, with three exceptions.** Inside `internal/` a
capitalized name is a promise to the other packages of this module, so about
seventy names that no other package read went lowercase. Enum constants of an
exported type stay exported, `Err` sentinels stay exported because a sentinel
exists to be matched, and `signing.Sign`, `Verify`, `Compute` and `ParseHeader`
stay exported although only `VerifyAny` has an outside caller: a signing package
that exposes verification and hides signing is the odder shape.

**The integration tag is drawn at what a test destroys or demands, not what it
touches.** A test that flushes a shared Redis database, or creates and drops a
PostgreSQL one, carries `//go:build integration`; `make go-test-integration`
runs those. Tests that create uniquely named queues or secrets and delete
exactly those keep their environment-variable gate, because they destroy nothing
pre-existing. The point of the split is that `make go-test` needs no privilege
beyond the application's own tables — which is a property only if it is applied
everywhere, so a half-tagged state was worse than either alternative.

**Concurrency is for calls that are independent *and* free of an ordering or a
budget.** `FactSource.For` made six round trips in a row to assemble one
dispute's record, and only the first was a real dependency: customer history,
precedent and base rates are all keyed on what the dispute row says. The other
four run in one `errgroup` now — 469 ms to 380 ms on the configuration that
ships, measured in `docs/measurements/2026-09-15-facts-assembly.txt`. Two
properties hold the shape up. The optional branches return `nil` rather than
their error, because a goroutine in an `errgroup.WithContext` that returns
non-nil cancels its siblings, and a failed base-rate query would have taken down
the evidence read the record cannot do without; retrieval and base rates already
degraded rather than failed, and that behaviour is now load-bearing rather than
merely kind. And each branch writes its own local, with `Facts` assembled after
`Wait` — distinct fields of one shared struct would be safe too, and it is the
kind of safe that stops being true the first time two branches touch one field,
on a day the race detector has nothing to say about.

**The prize was fixed before the change was written: a fan-out removes the sum
of every branch except the slowest.** Here the embedding call is roughly 400 ms
and the other three reads together roughly 70 ms, so 70-odd milliseconds was the
whole of it and Voyage set the floor either way. On loopback this is a small
win. It is kept because every one of those branches is a network call anywhere
that is not this laptop.

**Four sequential loops stay sequential, and three of them fail the second half
of that rule rather than the first.** `readEvidence` carries a rune budget that
shrinks as files are read in upload order, so which files reach the record is
decided by the record and not by which S3 call finished first. `drain` in
`cmd/agent` and `Runner.Run` in `internal/agent/eval` each check a running cost
total before the next call, and a ceiling means nothing with ten calls already
in flight past it. `drainOnce` in `internal/outbox` publishes in id order and
commits the prefix that succeeded, which concurrency turns into an arbitrary set
of successes and no clean `id = ANY($1)`. Independence is the easy half of the
test; the invariant is the half that decides.

**A degraded record says so, at `warn`.** The base-rate branch swallowed its
error into `rates = nil` with nothing logged. Retrieval gets away with the same
shape because it writes the failure into `Retrieval.Note`, where a reader of
`agent_runs` finds it; base rates have no such field, so a broken aggregate
would have cost every subsequent draft a paragraph of context in silence.
`FactSourceOptions` takes a `Logger` now. `warn` rather than `error` because the
run succeeds and the letter is written, and because an error record becomes a
Sentry event once a DSN is set — one flaky query would be one event per dispute
drafted. The line carries the dispute, the merchant, the reason code and the
kind, which are the query's own arguments; the cardholder claim and the customer
reference are not on it. It is skipped entirely when the group context is
already cancelled, because that means a sibling failed first and logging a
degradation that never happened would bury the real cause.

---

## Observability

**Sentry, for both halves, because Datadog's free tier cannot answer the
question.** Datadog gives away host metrics for five hosts with one day of
retention and no error tracking at all; the question was how exceptions get
captured. Sentry's Developer plan is 5,000 errors a month and covers Go and the
browser. OpenTelemetry remains the vendor-neutral answer and the better one to
learn, but the library that does this specific job in an afternoon is the
easier one to walk back, so it went first.

**An unset DSN is not a quieter path, it is no path.** `sentry.Init` is never
called, the client stays nil, and that nil is the switch every other piece
reads. Same logs, same status codes, same shutdown. That is how this runs on a
laptop and the property is asserted, not assumed: with no DSN, a captured
exception and a flush leave a local HTTP server's request log empty.

**The official `slog` bridge was not used.** It emits Sentry Logs rather than
events, so `logger.Error` would never become an issue, and it replaces the
handler rather than wrapping one, which would have cost a fan-out dependency to
keep the stdout JSON logs. A handler in `boot` does the job with no extra
module.

**Flush cannot be a deferred closure.** Every main ends with a fatal log line
and `os.Exit`, which skips defers, so the event that matters most would be
captured and discarded. `boot.FlushSentry` is a named function, deferred for the
clean path and called explicitly after the fatal line.

**`DisableTelemetryBuffer` is on, because the default lost events.** v0.49 queues
into a buffer drained on a tick and `Flush` returns success before the scheduler
has taken the event, so the envelope leaves after the process was meant to be
gone. Measured: zero events on the default path, one on the other.

**The Sentry middleware wraps `Observe` from outside, with `Repanic`.** Inside
would put two recoverers in the chain — Sentry reporting before a request id
exists, then `Observe` logging the repanic and the bridge reporting it again.
Outside, `Observe`'s recover is still the only one that fires, and Sentry's
contribution is the per-request hub that gives the event its request id.

**Headers are not sent at all, rather than filtered.** The SDK keeps every
header and scrubs a denylist that does not contain `X-Processor-Signature`. A
denylist that must stay right about every header this platform ever adds is the
wrong shape for "no secret may be sent".

**Nothing below error is captured, not even breadcrumbs.** A breadcrumb hangs off
a scope, and with eight concurrent dispute handlers on a process-wide scope one
dispute's event would carry another's trail — history that reads as causal and
is not.

**The dashboard sends no URLs, no replay and no input.** Filters are mirrored
into the query string on every keystroke, so the page URL carries the operator's
free-text search and the merchant names they typed. Query and fragment are
stripped from events and breadcrumbs, console and `ui.input` breadcrumbs are
dropped, and session replay is never added.

**No route timing in the dashboard, deliberately.** Sentry's browser tracing
propagates `sentry-trace` and `baggage` to anything matching localhost, which is
every call to the read API on another port — and that API answers preflight with
`Access-Control-Allow-Headers: Content-Type`. Setting a DSN would have stopped
the dashboard loading any data. When tracing is actually wanted, the integration
and the CORS allow-list change in the same commit.

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
- **`unset VOYAGE_API_KEY` does not turn the embedder off.** `config.Load` reads
  the process environment first and falls back to `.env`, and only a variable
  *present* in the environment shadows the file — so an unset name reads
  straight through to `.env`. `export VOYAGE_API_KEY=""` is what disables it. An
  afternoon of "lexical" measurements were vector measurements.
- **Voyage rate-limits on the free tier, and the client waits 25 s per retry**
  (`internal/llm/embed.go`, three attempts). Back-to-back runs of anything that
  embeds read 25,5xx ms or 50,7xx ms, which are retry counts and not latency —
  the first attempt at the assembly table showed a 2x speedup that was purely
  which binary got throttled harder. Space repeated runs 40 s apart.
