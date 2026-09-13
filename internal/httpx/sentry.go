package httpx

import (
	"net/http"

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
)

// Sentry gives each request a Sentry hub of its own, so that anything reported
// while it is being served carries that request's scope instead of one shared
// with every other request in flight.
//
// It goes outside Observe, and the order is the whole design. Observe already
// recovers panics, logs them and answers 500, and that behaviour does not
// change. Outside, sentryhttp's recover never fires - Observe has already
// eaten the panic - so there is exactly one recover in the chain and exactly
// one event per panic: the one the slog bridge makes from Observe's own log
// line, which finds this hub on the context and so arrives with the request id
// on it. Inside, the order reverses and both recovers run: sentryhttp reports
// the panic first, with no request id because Observe has not run yet, and
// then Observe reports it again through the bridge. Two issues for one panic,
// one of them unattributable.
//
// Repanic is set because sentryhttp's recover is a backstop for any route that
// is ever served without Observe in front of it. A backstop that swallowed the
// panic would turn a crash into a 200 with an empty body, which is further
// from stock net/http than the crash.
//
// With no DSN there is no client and a request never enters any of this: it
// goes straight to next, with no hub, no scope and no wrapped writer.
func Sentry() func(http.Handler) http.Handler {
	handler := sentryhttp.New(sentryhttp.Options{Repanic: true})
	return func(next http.Handler) http.Handler {
		instrumented := handler.Handle(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if sentry.CurrentHub().Client() == nil {
				next.ServeHTTP(w, r)
				return
			}
			instrumented.ServeHTTP(w, r)
		})
	}
}

// tagRequestID puts the request id on the hub Sentry left on the context, if
// it left one. Nothing happens when Sentry is off, which is when there is no
// hub to find.
func tagRequestID(r *http.Request, id string) {
	if hub := sentry.GetHubFromContext(r.Context()); hub != nil {
		hub.Scope().SetTag("request_id", id)
	}
}
