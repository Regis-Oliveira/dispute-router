package worker

import (
	"context"
	"strconv"
	"time"

	"github.com/getsentry/sentry-go"
)

// sentryFlushTimeout matches the SDK's own default. It is only ever waited on
// once, on the way out of a panic that is about to end the process.
const sentryFlushTimeout = 2 * time.Second

// observeDispute scopes ctx to one dispute for Sentry, and returns the
// function to defer.
//
// Two things happen here, and one deliberately does not.
//
// The dispute id goes on a hub belonging to this goroutine alone, and that hub
// goes on the context. Everything reported under ctx therefore carries the id
// as a tag - including each of the ErrorContext calls in handle, which reach
// Sentry as events by way of the slog bridge in cmd/internal/boot.
//
// A panic is reported and then re-panicked. A panic in one of these goroutines
// kills the process today, and that is the behaviour to keep; what changes is
// that it stops being silent. The flush before the re-panic is the difference
// between reporting it and losing it: the transport batches, and the process
// is about to end.
//
// What does not happen is a second capture of the failures handle already
// logs. Every one of those is an ErrorContext on a logger that carries
// dispute_id, so the bridge already turns it into an event with this tag on
// it. Capturing here as well would send two events for one failure - twice the
// free tier's error budget, and two issues to triage for one problem - and the
// second would say strictly less than the first, because it would not have the
// message that says which step failed.
//
// With no DSN there is no client: ctx comes back untouched and the deferred
// function does nothing, so there is no hub, no recover, and no change to how
// a panic leaves the process.
func observeDispute(ctx context.Context, disputeID int64) (context.Context, func()) {
	if sentry.CurrentHub().Client() == nil {
		return ctx, func() {}
	}

	hub := sentry.CurrentHub().Clone()
	hub.Scope().SetTag("dispute_id", strconv.FormatInt(disputeID, 10))
	ctx = sentry.SetHubOnContext(ctx, hub)

	return ctx, func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		hub.RecoverWithContext(ctx, recovered)
		hub.Flush(sentryFlushTimeout)
		panic(recovered)
	}
}
