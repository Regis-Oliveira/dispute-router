package boot

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"time"

	"github.com/getsentry/sentry-go"
)

// flushTimeout bounds the wait when a process is on its way out. It is the
// SDK's own default and the same trade every Sentry SDK makes: long enough for
// one round trip to the ingest endpoint, short enough that Sentry being down
// cannot hold a shutdown open.
const flushTimeout = 2 * time.Second

// Sentry starts exception reporting for this process.
//
// An empty dsn is not an error. It is the local default, and it means the SDK
// is never initialised at all: no client, no background goroutine, no buffer.
// Everything downstream asks sentry.CurrentHub().Client() whether there is a
// client before it does anything, so with no DSN the reporting code is not
// merely quiet - it does not run, and a laptop gets the same logs, the same
// status codes and the same shutdown it got before any of this existed.
//
// A dsn that is set but malformed is an error, which is the rule the config
// package follows for every other setting: a value present but unparseable is
// a startup failure, not a silent default. The parse lives here rather than in
// config because the grammar belongs to the SDK.
func Sentry(dsn, environment, release string) error {
	if dsn == "" {
		return nil
	}

	err := sentry.Init(sentry.ClientOptions{
		Dsn:         dsn,
		Environment: environment,
		Release:     release,

		// The older of the SDK's two delivery paths, because it is the one
		// whose Flush means what this package needs it to mean.
		//
		// v0.49's default path puts an event into a telemetry buffer that a
		// scheduler goroutine drains on a hundred-millisecond tick, and Flush
		// can return "buffer flushed successfully" before the scheduler has
		// picked the event up: the envelope goes out a tick later, which is a
		// tick after the process was supposed to have ended. Measured, not
		// assumed - sentry_test.go fails on the default path and passes on
		// this one. Everything here is built on flush-then-exit, so the path
		// where flush is a request rather than a guarantee is the wrong one.
		DisableTelemetryBuffer: true,

		// Exceptions only. Tracing would send a transaction for every request
		// the api and ingest services answer, which spends the free tier's
		// performance budget in an afternoon on a question nobody has asked of
		// this project yet.
		EnableTracing: false,

		// Nothing is taken off a request except its method and path.
		//
		// The alternative is the SDK's default, which collects every header
		// and scrubs the values whose names it recognises as sensitive. It
		// does not recognise X-Processor-Signature, and a denylist that has to
		// be right about every header this platform will ever add is the wrong
		// shape for "no secret leaves the process". Sending nothing and
		// tagging the request id is the right shape: the id is the join back
		// to the log line, which already carries what is safe to carry.
		//
		// This is the whole answer, not half of it. SendDefaultPII is the
		// older, coarser switch and the SDK ignores it entirely once this
		// field is set.
		DataCollection: &sentry.DataCollection{
			UserInfo:    sentry.Set(false),
			Cookies:     &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
			QueryParams: &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
			HTTPHeaders: &sentry.HeaderCollectionConfig{
				Request:  &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
				Response: &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
			},
			// Empty rather than absent: absent means "collect them all".
			HTTPBodies: []sentry.BodyType{},
		},
	})
	if err != nil {
		return fmt.Errorf("start sentry: %w", err)
	}
	return nil
}

// FlushSentry sends whatever the SDK has buffered. It is safe to call when
// Sentry was never started, and safe to call twice.
//
// It is a named function rather than something Sentry hands back because of
// where it has to be called from. Sentry batches, so an event only leaves the
// process on a flush; every main here ends in os.Exit, which skips deferred
// calls; and the flush that matters most is the one after main logs the fatal
// error, which is below run and cannot see anything run returned.
func FlushSentry() {
	sentry.Flush(flushTimeout)
}

// sentryTags are the log fields promoted from the event's context to a Sentry
// tag, which is the difference between a field you can read once you have
// found the event and a field you can search and group by.
//
// Three, because these are the identifiers this platform navigates by: a
// dispute, one delivery of one HTTP request, and which try of a retried thing
// this was. Everything else is on the event too, under contexts.log.
var sentryTags = map[string]bool{
	"dispute_id": true,
	"request_id": true,
	"attempt":    true,
}

// sentryHandler passes every record through to next unchanged and, for errors
// only, also captures a Sentry event carrying the record's fields.
//
// Errors only, and nothing below them becomes a breadcrumb either. Breadcrumbs
// hang off a Sentry scope, and the scope most records here are written under
// is the process-wide one: the worker decides eight disputes at once and the
// api serves many requests at once, so a single ring buffer of everything
// logged would attach one dispute's event to another dispute's trail. That
// looks like causal history and is not, which is worse than having none. Where
// there is a per-request or per-dispute scope the trail would be right, but it
// would also be the log line over again - and the event already carries the
// request id or dispute id that finds those log lines.
type sentryHandler struct {
	next slog.Handler

	// attrs are the ones added with Logger.With, already qualified by the
	// groups that were in force when they were added. slog keeps them inside
	// next, where this handler cannot read them, and they are exactly the
	// fields worth having: the worker's dispute_id is attached this way.
	attrs []slog.Attr

	// prefix is the dotted group path applied to a record's own attributes.
	prefix string
}

func (h sentryHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h sentryHandler) Handle(ctx context.Context, record slog.Record) error {
	err := h.next.Handle(ctx, record)
	if record.Level >= slog.LevelError && sentry.CurrentHub().Client() != nil {
		h.capture(ctx, record)
	}
	return err
}

func (h sentryHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	out := h
	out.next = h.next.WithAttrs(attrs)
	// A fresh slice rather than an append onto h.attrs: two loggers derived
	// from the same parent would otherwise write into one backing array and
	// each would see the other's fields.
	out.attrs = make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	out.attrs = append(out.attrs, h.attrs...)
	for _, attr := range attrs {
		attr.Key = h.prefix + attr.Key
		out.attrs = append(out.attrs, attr)
	}
	return out
}

func (h sentryHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	out := h
	out.next = h.next.WithGroup(name)
	out.prefix = h.prefix + name + "."
	return out
}

// capture turns one error record into one Sentry event.
func (h sentryHandler) capture(ctx context.Context, record slog.Record) {
	// The hub a request or a dispute handler put on the context, if it did;
	// the process-wide one otherwise. The per-request hub is what carries the
	// request id, so this is how a panic's log line arrives tagged.
	hub := sentry.GetHubFromContext(ctx)
	if hub == nil {
		hub = sentry.CurrentHub()
	}

	event := sentry.NewEvent()
	event.Level = sentry.LevelError
	event.Logger = "slog"
	event.Message = record.Message
	event.Timestamp = record.Time

	// Grouped by the message, which in this repository is always a constant
	// ("apply failed", "handler panicked"). Left to Sentry's default the
	// grouping would key off the error's text instead, and "load dispute 4821:
	// connection refused" makes one issue per dispute.
	event.Fingerprint = []string{record.Message}

	fields := sentry.Context{}
	var failure error

	collect := func(prefix string, attr slog.Attr) {
		h.collect(event, fields, prefix, attr, &failure)
	}
	for _, attr := range h.attrs {
		collect("", attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		collect(h.prefix, attr)
		return true
	})

	// Where the line was written. slog records the caller's program counter on
	// every record, so this is the logger.Error call itself and not a frame
	// inside this handler.
	if record.PC != 0 {
		frame, _ := runtime.CallersFrames([]uintptr{record.PC}).Next()
		fields["source"] = fmt.Sprintf("%s:%d", frame.File, frame.Line)
	}
	event.Contexts["log"] = fields

	// An error field becomes the exception, so the event has a subtitle worth
	// reading and Sentry can tell two failures of the same operation apart.
	if failure != nil {
		event.Exception = []sentry.Exception{{
			Type:  fmt.Sprintf("%T", failure),
			Value: failure.Error(),
		}}
	}

	hub.CaptureEvent(event)
}

// collect adds one attribute to the event, flattening groups onto dotted keys
// and remembering the first error it sees.
func (h sentryHandler) collect(event *sentry.Event, fields sentry.Context, prefix string, attr slog.Attr, failure *error) {
	value := attr.Value.Resolve()
	key := prefix + attr.Key

	if value.Kind() == slog.KindGroup {
		for _, inner := range value.Group() {
			h.collect(event, fields, key+".", inner, failure)
		}
		return
	}

	if err, ok := value.Any().(error); ok && *failure == nil {
		*failure = err
	}
	if sentryTags[key] {
		event.Tags[key] = value.String()
	}

	// Anything slog could not classify is rendered the way the log line
	// renders it. The event is JSON on the wire, and an error or a typed
	// constant handed over as itself encodes as {} - the field would be on the
	// event and empty, which is the one outcome worse than it being absent.
	if value.Kind() == slog.KindAny {
		fields[key] = value.String()
		return
	}
	fields[key] = value.Any()
}
