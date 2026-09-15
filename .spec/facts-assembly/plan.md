# Assembling the record concurrently

Status: **built and measured (2026-09-15).** Numbers in
`docs/measurements/2026-09-15-facts-assembly.txt`: 469 ms -> 380 ms on the
configuration that ships. Step 5 is the only thing outstanding - the two
decisions below have not moved to `docs/DECISIONS.md` yet, so this plan is not
deletable.

`FactSource.For` (`internal/agent/facts.go`) is the only place in the project
that makes several independent round trips in a row and waits for each one
before starting the next. Everywhere else that shape occurs the project already
fans out with `errgroup` — `internal/api/summary.go`, `internal/api/store.go`,
`internal/toolloop/loop.go`, `internal/worker/worker.go`. This plan closes the
gap, and writes down why the four *other* sequential loops in the project stay
sequential, because "it is I/O, so parallelise it" is the wrong rule and this
repo is a place to learn the right one.

---

## What is sequential today

Six round trips, in order, for one dispute:

| # | Call | Talks to | Needs |
|---|------|----------|-------|
| 1 | `tools.DisputeWithClaim` | Postgres | the dispute id |
| 2 | `tools.CustomerHistory` | Postgres | 1 — merchant, customer ref |
| 3 | `evidence.List` | S3 | the dispute id |
| 4 | `readEvidence` | S3, N files | 3 |
| 5 | `retriever.For` | Voyage, then Postgres | 1 — merchant, claim |
| 6 | `baseRates` | Postgres | 1 — merchant, reason code, kind |

Only **1** is a real dependency. Once the dispute row is in hand, 2, 3–4, 5 and
6 know everything they need and none of them reads what another writes. Today
the assistant waits through all of them in series before it buys its first
token, and the wait is on the clock of a chargeback deadline.

Call 5 is the expensive one: it is an embedding API call over the network
*followed by* a vector query, which is why it goes last today and why it is the
one most worth overlapping with the others.

## The shape

One dependency stage, then one `errgroup`:

```
DisputeWithClaim                      (serial — everything below needs it)
  ├── CustomerHistory                 fatal on error
  ├── evidence.List → readEvidence    fatal on error
  ├── retriever.For                   degrades: RetrievalFailed
  └── baseRates                       degrades: nil rates
```

Four goroutines, no `SetLimit`. Unlike `toolloop`, where the model chooses how
many tools to ask for, the fan-out here is fixed at four by the code, and three
of them are one pool query each. There is nothing to bound.

### Errors

Two of the four are already non-fatal today and stay that way — retrieval
failing is not the record failing, and the existing comments say why. That
matters more here than it looks: an `errgroup.WithContext` goroutine that
returns non-nil **cancels its siblings**, so the optional two must keep
swallowing their own errors into a degraded value and return `nil`. They do
already; the change must not "tidy" that into a returned error.

The two fatal ones keep their wrapped messages (`fmt.Errorf("customer history
for dispute %d: %w", …)`), and cancelling the siblings when one of them fails
is the behaviour we want: the record has already failed, so there is no reason
to keep paying Voyage for an embedding nobody will read.

### Writing the results

Each goroutine writes to its **own local variable**, and `Facts` is assembled
from them after `Wait` returns. Writing into distinct fields of one shared
struct would also be safe, and is the kind of safe that stops being true the
first time someone adds a field two goroutines both touch — the race detector
would not catch the day it broke. Same reasoning as `runTools`'s write-by-index
in `internal/toolloop/loop.go`, one size down.

## What stays sequential, and why

Written down here so the next reading of this code does not "finish the job".

- **`readEvidence` (`internal/agent/evidence.go`).** N independent S3 GETs, and
  the obvious next candidate. It carries a rune budget that shrinks as files are
  read, in upload order, and the const block above it already states the
  invariant: *which files were read is decided by the record and not by which S3
  call finished first.* Fetching concurrently and applying the budget in order
  afterwards would preserve that, at the price of downloading bytes to discard.
  Not worth it until a dispute with many large files exists to measure.
- **`drain` (`cmd/agent/main.go`).** Sequential because the calls cost money and
  a batch exists to bound the spend.
- **`Runner.Run` (`internal/agent/eval/run.go`).** Checks a running cost total
  before each case; a ceiling means nothing with ten cases already in flight
  past it.
- **`drainOnce` (`internal/outbox/relay.go`).** Publishes in id order and
  commits the prefix that succeeded. Concurrency turns that into an arbitrary
  set of successes.

The pattern: fan out when the calls are independent **and** free of an ordering
or budget invariant. Three of the four above fail the second test, not the
first.

## Measuring it

`Trace.Assembly` already records the wall time of `For` on every run, so the
number exists before the change is made. `cmd/agent -prompt -dispute <id>`
assembles the record and stops without buying a token, which makes the
before/after free to collect.

Take both on the same dispute, one with evidence on file and precedent
available, so all four branches actually do work. Record it in
`docs/measurements/`.

`-prompt` had to learn to say how long it took; it logs `assembly_ms` now.

**Space the runs.** Back-to-back vector runs are not measurable: Voyage
rate-limits the key and `internal/llm/embed.go` pauses a flat 25 s per retry,
three attempts, so a throttled assembly reads 25,5xx or 50,7xx ms. Forty seconds
between invocations was enough. And `unset VOYAGE_API_KEY` does **not** turn the
embedder off - `config.Load` falls back to `.env`, and only a variable present
in the process environment shadows the file. Export it empty.

## Steps

1. Capture `Assembly` before the change, on a named dispute id.
2. Rewrite `FactSource.For` as above. `internal/agent/facts.go` only.
3. `make go-test` (`-race` — the point of the exercise), `make go-lint`,
   `gofmt -l .`.
4. Capture `Assembly` after, same dispute id.
5. Move the two decisions above — the fan-out, and the four refusals — into
   `docs/DECISIONS.md`, and delete this plan.

Cost: free. No model call is added or removed; the embedding call in 5 happens
either way.
