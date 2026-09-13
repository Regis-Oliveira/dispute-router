# Observability

Status: **not built, and not yet decided.** This is the last phase of the
2026-09-13 Go review. Phases 0 to 5 shipped; what they decided lives in
`docs/DECISIONS.md` under "Go shape", the rules in `CLAUDE.md`, and the
reasoning in `docs/go-conventions.md`. Their plan is deleted, because git is
the archive.

The question this answers: how do exceptions get captured, and where does
observability live, for the Go services and for the Angular dashboard.

## What the free tiers actually are, checked 2026-09-13

- **Sentry, Developer plan.** Free: 5,000 errors and 10,000 performance units a
  month, 30-day retention, one user, plus small allowances for logs, spans,
  replays, and one cron and one uptime monitor. It stops accepting events at the
  cap rather than billing for the overage.
- **Datadog, free plan.** Infrastructure metrics for up to five hosts with one
  day of retention. No APM, no log management, no real user monitoring — those
  need a paid plan, with a 14-day trial.

So Datadog's free tier cannot answer the question. It carries host metrics, and
the question is about exceptions. Sentry covers both halves at no cost, which
makes it the one worth trying here.

## 6.1 Go

`github.com/getsentry/sentry-go`. One `boot.Sentry(dsn)` beside the other
helpers in `cmd/internal/boot`, called by every main. A `slog` handler so
`logger.Error` becomes an event carrying the structured fields already being
logged. `sentryhttp` in `internal/httpx` beside `Observe`, so a recovered panic
arrives with its request id attached. The worker captures around each dispute
handler with the dispute id as a tag.

An unset DSN means disabled, so the local default changes nothing.

## 6.2 Angular

`@sentry/angular`: `Sentry.init` in `main.ts`, the `ErrorHandler` provider, and
`TraceService` for route timing. The DSN lives in `environment.ts`; empty means
off.

## 6.3 The alternative worth understanding first

OpenTelemetry (`go.opentelemetry.io/otel` plus an OTLP exporter) sends traces
and metrics to any backend, including Sentry, and including a Jaeger or Grafana
running locally. More to learn and no hosted free tier needed, against a library
that does the specific job in an afternoon.

The two are not exclusive: instrumenting with OpenTelemetry and pointing it at
Sentry is a third option, and the reason to know that before choosing is that
the vendor library is the harder one to walk back.

## What "done" looks like

A deliberate panic in a handler and a `logger.Error` in the worker both appear
in the project, with the request id and the dispute id on them. The dashboard
reports a thrown error. Then the event counts go in `docs/measurements/`, and
the decision that was actually taken moves to `docs/DECISIONS.md`.

## Open questions for the developer

1. Sentry, OpenTelemetry, or OpenTelemetry pointed at Sentry.
2. Whether a hosted service should see this data at all. It is a study project
   on a laptop, and a local Jaeger keeps everything on the machine. That is a
   real answer, not a dodge, and it changes the recommendation.
3. If Sentry: whether the dashboard half is wanted, since it means a DSN in
   client-side code and a third party seeing browser errors.
