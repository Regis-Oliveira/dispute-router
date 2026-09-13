package worker

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

// flushTestTimeout is generous, because half of what these tests assert is the
// absence of traffic and a flush that gave up early would make that meaningless.
const flushTestTimeout = 5 * time.Second

// A DSN is a URL with a key in front of it, so an httptest server named by one
// sees exactly what Sentry would have seen.
type sentrySink struct {
	dsn string

	mu     sync.Mutex
	bodies [][]byte
}

func newSentrySink(t *testing.T) *sentrySink {
	t.Helper()

	sink := &sentrySink{}
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		sink.mu.Lock()
		sink.bodies = append(sink.bodies, body)
		sink.mu.Unlock()
	}))
	t.Cleanup(server.Close)

	sink.dsn = "http://publickey@" + strings.TrimPrefix(server.URL, "http://") + "/1"
	return sink
}

func (s *sentrySink) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// tags returns the tags of every event item in every envelope received. An
// envelope is a header line followed by pairs of item-header and item-body
// lines.
func (s *sentrySink) tags(t *testing.T) []map[string]string {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	var tags []map[string]string
	for _, body := range s.bodies {
		lines := bytes.Split(bytes.TrimRight(body, "\n"), []byte("\n"))
		for i := 1; i+1 < len(lines); i += 2 {
			var item struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(lines[i], &item); err != nil {
				t.Fatalf("envelope item header %q: %v", lines[i], err)
			}
			if item.Type != "event" {
				continue
			}
			var event struct {
				Tags map[string]string `json:"tags"`
			}
			if err := json.Unmarshal(lines[i+1], &event); err != nil {
				t.Fatalf("envelope event body %q: %v", lines[i+1], err)
			}
			tags = append(tags, event.Tags)
		}
	}
	return tags
}

// startSentry points the SDK at dsn for one test. The empty string means off.
func startSentry(t *testing.T, dsn string) {
	t.Helper()

	previous := sentry.CurrentHub().Client()
	t.Cleanup(func() { sentry.CurrentHub().BindClient(previous) })
	sentry.CurrentHub().BindClient(nil)

	if dsn == "" {
		return
	}
	client, err := sentry.NewClient(sentry.ClientOptions{Dsn: dsn, DisableTelemetryBuffer: true})
	if err != nil {
		t.Fatalf("sentry client: %v", err)
	}
	sentry.CurrentHub().BindClient(client)
}

// Anything reported while one dispute is being handled carries its id, which is
// what makes an event findable from the dispute and the dispute findable from
// the event.
func TestObserveDisputeTagsWhatIsReportedUnderIt(t *testing.T) {
	sink := newSentrySink(t)
	startSentry(t, sink.dsn)

	ctx, done := observeDispute(t.Context(), 4821)
	hub := sentry.GetHubFromContext(ctx)
	if hub == nil {
		t.Fatal("no hub on the context, so nothing reported here would be attributed")
	}
	hub.CaptureMessage("something the handler noticed")
	done()

	sentry.Flush(flushTestTimeout)

	tags := sink.tags(t)
	if len(tags) != 1 {
		t.Fatalf("got %d events, want 1: %v", len(tags), tags)
	}
	if tags[0]["dispute_id"] != "4821" {
		t.Errorf("tag dispute_id = %q, want 4821", tags[0]["dispute_id"])
	}
}

// A panic in a dispute handler kills the process today. It still does - it is
// only no longer silent, and the flush is what makes the difference between
// reporting it and losing it on the way out.
func TestObserveDisputeReportsAPanicAndLetsItThrough(t *testing.T) {
	sink := newSentrySink(t)
	startSentry(t, sink.dsn)

	panicked := func() (recovered any) {
		defer func() { recovered = recover() }()

		_, done := observeDispute(t.Context(), 4821)
		defer done()
		panic("kaboom")
	}()

	if panicked != "kaboom" {
		t.Fatalf("the panic did not come back out: got %v, want kaboom", panicked)
	}

	// No flush here on purpose: observeDispute has to have done it, because the
	// process it is protecting is on its way down.
	tags := sink.tags(t)
	if len(tags) != 1 {
		t.Fatalf("got %d events, want 1: %v", len(tags), tags)
	}
	if tags[0]["dispute_id"] != "4821" {
		t.Errorf("tag dispute_id = %q, want 4821", tags[0]["dispute_id"])
	}
}

// With no DSN nothing is installed: the context comes back as it went in, the
// deferred function is inert, and a panic leaves exactly as it did before any
// of this existed.
func TestObserveDisputeDoesNothingWhenDisabled(t *testing.T) {
	sink := newSentrySink(t)
	startSentry(t, "")

	ctx, done := observeDispute(t.Context(), 4821)
	if ctx != t.Context() {
		t.Error("the context was replaced with Sentry off")
	}
	if sentry.GetHubFromContext(ctx) != nil {
		t.Error("a hub was installed with Sentry off")
	}
	done()

	panicked := func() (recovered any) {
		defer func() { recovered = recover() }()

		_, done := observeDispute(t.Context(), 4821)
		defer done()
		panic("kaboom")
	}()
	if panicked != "kaboom" {
		t.Fatalf("the panic did not come back out: got %v, want kaboom", panicked)
	}

	sentry.Flush(flushTestTimeout)
	if n := sink.requests(); n != 0 {
		t.Errorf("the disabled SDK made %d HTTP requests, want 0", n)
	}
}
