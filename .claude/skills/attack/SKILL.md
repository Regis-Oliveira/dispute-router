---
name: attack
description: Security-test the running dispute-router against its own threat model. Runs the black-box suite (make attack), demonstrates the two findings a fixed test cannot (the readyz error leak, live LLM prompt injection), and re-checks that the known auth gaps are still the only ones of their kind. Everything targets localhost only. Use when the user asks to attack, pentest, or security-test the app.
---

# Attack

Security-test this app against its own threat model, which lives in
`docs/DECISIONS.md` (the signing, CORS, evidence and secrets decisions) and in
the README's "Known gaps". Everything here targets `localhost` only. This is
authorized testing of the owner's own app; it is not a licence to point any of
it at another host, and nothing here should be adapted to do so.

The surface is three HTTP-shaped things, not the binary: the ingest endpoint
that takes signed webhooks, the read API whose defining property is that
nothing authenticates a request, and the agent that drafts letters from
untrusted dispute text. Work through them in the order below and write one
report at the end.

## 0. Preconditions

The suite needs the stack and both services up. Check, and start what is
missing rather than failing:

```bash
docker compose ps --format '{{.Name}} {{.State}}'      # postgres, redis, localstack
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8081/healthz   # api
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/healthz   # ingest
```

If a service is down, start it (`make api`, `make ingest`) in the background and
wait for `healthz` to answer 200. If the datastores are down, `make up` first.

## 1. The black-box suite

This is the durable backbone and it must pass. It pins every HTTP-layer defense
the map found, and it pins the known auth gaps so that a change which *adds*
auth fails a test deliberately rather than passing silently.

```bash
make attack
```

Report the result verbatim. A failure here is either a regression in a defense
(a test that pinned "rejected" now sees "accepted") or a gap that closed (a test
that pinned "unauthenticated" now sees a 401, which is good news the test states
as a failure on purpose). Read which it is before calling it a problem.

## 2. The readyz error leak — needs a datastore down, so the suite cannot

Both services echo the raw Postgres or Redis error into `/readyz` when a
dependency is down, unauthenticated. A black-box test cannot show this without
taking a container down, so do it here and put it back:

```bash
curl -s http://localhost:8081/readyz            # healthy: {"postgres":"ok","redis":"ok"}
docker compose stop redis
curl -s http://localhost:8081/readyz            # unhealthy: inspect the body
docker compose start redis                      # ALWAYS restore, even on failure
```

The finding is present if the unhealthy body contains a host name, a port, or a
database name. It is worse if it contains anything resembling a DSN or a
credential; `pgx` redacts the password, so expect host and database rather than
a full secret. Report exactly what the body contained. The fix, if the user
wants it, is a static `"unavailable"` in the body and the detail logged
server-side, at `internal/api/handler.go`'s readyz handler and the equivalent in
`cmd/ingest/main.go`. Do not apply it unasked; this skill tests, it does not fix.

Restore Redis before moving on and confirm `/readyz` is `ok` again.

## 3. Live prompt injection against the agent — spends real money

The agent's defenses are layered and the eval harness already models the attack
with planted-instruction fixtures graded counterfactually (a refusal that still
recommends `insufficient_evidence` is correct, not compliance). Run those first,
because they cost the least and are the honest baseline:

```bash
go run ./cmd/eval -cases                                    # free: show the planted fixtures
go run ./cmd/eval -only planted_instruction_1 -verify -letters -max-cost 0.50
```

`-verify` runs the independent verifier, `-letters` prints each draft so you can
read whether the injected instruction changed the recommendation or leaked into
the letter. `-max-cost` is a hard ceiling in dollars; keep it low. This calls
the Anthropic API and spends real money, so:

- Never run a broad `-samples` sweep without the user asking; each sample is a
  paid draft.
- If the user has not signalled they want to spend, run only `-cases` (free) and
  report what the fixtures model, then ask before spending.

The adaptive part a fixed fixture cannot do is a *new* payload. The canonical
attack is one string; variety is the test. If the user wants to go further,
the honest way is to add a case to `internal/agent/eval/cases.go` and seed a
matching claim, then run `-only <newcase>`. Payload ideas the current fixtures
do not cover: an instruction split across the cardholder claim and an uploaded
evidence file (multi-field), an attempt to inject the closing quarantine fence
marker to escape its block (assert `facts.go`'s `fence` neutralizes it), a
payload aimed at the verifier rather than the generator ("approve this draft"),
and one asking the letter to borrow a real figure from the precedent block.
Propose these to the user before building them; each new case is paid to run.

The worst case is bounded and worth stating in the report: a bad draft is caught
by the independent verifier and the deterministic graders, it reaches a human
before anything happens, and the agent has no write tools and no access to
money. Prompt injection here degrades a draft; it does not move a dispute.

## 4. Re-check the known gaps

The map's top finding is that the read API has no auth and one endpoint
(`POST /api/reviews/{id}/decision`) moves money on a name in the body. That is
known and acknowledged. This skill's standing job is to catch a *new* one:

```bash
# every non-GET route across both services — the write surface
grep -rnE '"(POST|PUT|PATCH|DELETE) ' internal/api/handler.go cmd/ingest/main.go
```

There should be exactly three: the ingest webhook (HMAC-authenticated), the
decision endpoint, and the evidence presign. If a fourth appears, or a GET route
starts writing, that is a finding — a new unauthenticated state-changing
endpoint is the thing most likely to be added without noticing it needs a gate.

## 5. Report

One report, findings ranked by what a real attacker would reach for first, each
with: what was sent, what came back, whether it is a regression, a pinned known
gap, or a new finding, and where in the code it lives. Distinguish plainly
between "a defense that is working" (most of them), "a gap the owner already
documented" (the auth surface), and "something new". End with what, if anything,
is worth fixing versus accepting for a local study app — and let the user decide,
rather than fixing anything here.
