# Brief for a reviewer who did not write this

A study project: a miniature chargeback platform, built to learn Go, Angular,
Redis and AWS on top of Node/TypeScript/Postgres. Local only. The last stretch
of work added an agentic harness that drafts dispute rebuttals for human review.

The point of a review here is to find what the author could not, so this brief
exists to stop you spending the effort on things already known. Everything in
"Already known" is documented and deliberate; finding it again is not a finding.

## Where the reasoning lives

| file | what it holds |
|---|---|
| `docs/DECISIONS.md` | every real decision, and what it was chosen *against* |
| `.spec/agent-harness/part-b.md` | the agent design, step by step, with what was measured |
| `docs/conceitos-pt.md` | the same ground in Portuguese, written for study |
| `README.md` | what exists and how to run it |
| commit messages | long on purpose; several carry the reasoning for a whole file |

The code comments carry the *why*. If a comment and this brief disagree, the
comment is more likely to be current.

## Already known, documented, and not worth reporting

- **No authentication anywhere.** `POST /api/reviews/{id}/decision` moves a
  dispute to `represented` and takes the reviewer's name from the request body.
  Known, in the README, and the first thing that would change before deployment.
- **Nothing is deployed.** The Terraform plans to 60 resources and has never
  been applied.
- **Nine eval cases is a smoke test, not a pass rate.** Hand-picked disputes,
  each exercising one thing.
- **The graders measure rule violations, not quality.** Nobody has labelled
  whether a letter is good, so "45 of 45 passed" says nothing about persuasion.
- **The seeded cardholder claims come from a pool of fifteen texts**, which
  makes lexical retrieval unusually easy and the RAG comparison local to this
  dataset.
- **Token prices are configuration and may be stale.** Every cost figure is
  only as right as two numbers in `.env`.
- **The demo drafts in `db/demo/` were written by hand**, not by a model.

## Where a fresh pair of eyes would be most useful

Not a task list — a map of where the author's confidence is least earned.

- **The prompts.** `generatorSystem` and `verifierSystem` in
  `internal/agent/generate.go` and `verify.go` were written and revised by the
  same person who wrote the graders that check their output. That is a closed
  loop.
- **The quarantine.** `quarantine()` in `internal/agent/facts.go` handles
  cardholder text. It was extended once, after retrieval turned out to smuggle
  the same class of text in by another door. There may be a third door.
- **The graders.** Three of them have fired on drafts that were *refusing* the
  thing they were accused of. The boundary between "decidable by comparison" and
  "needs reading" is drawn in `internal/agent/eval/grade.go`, and it has been
  drawn wrong twice.
- **The money path.** The project's rule is that money is divided exactly once,
  at the formatter. The prompt boundary was missed for weeks. Are there others?
- **The eval cases themselves.** They select disputes by SQL. One of them was
  silently selecting the same dispute as another; another was drawing from
  states the agent never sees.

## Running it

`make up` then `make migrate`, `make seed`, `make verify`. Tests need
`DATABASE_URL`. Anything reaching a model needs `ANTHROPIC_API_KEY`; retrieval
falls back to Postgres full-text search without `VOYAGE_API_KEY`.

`go run ./cmd/agent -dispute N -prompt` prints exactly what a model would be
given, and costs nothing. It is the fastest way to see what the system actually
does.
