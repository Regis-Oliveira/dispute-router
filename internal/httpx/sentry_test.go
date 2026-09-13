package httpx

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

// A DSN is a URL with a key in front of it, so an httptest server named by one
// sees exactly what Sentry would have seen. sentrySink is that server; it keeps
// the raw envelopes and parses them on the test's goroutine, because a handler
// goroutine may not call t.Fatal.
type sentrySink struct {
	dsn string

	mu     sync.Mutex
	bodies [][]byte
}

func newSentrySink(t *testing.T) *sentrySink {
	t.Helper()

	sink := &sentrySink{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
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

// tags returns the tags of every event item in every envelope received.
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

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// The request id Observe generates is on the hub before the handler runs, so
// anything reported while serving the request carries it.
func TestRequestIDIsOnTheHub(t *testing.T) {
	sink := newSentrySink(t)
	startSentry(t, sink.dsn)

	reporting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub := sentry.GetHubFromContext(r.Context())
		if hub == nil {
			t.Error("no hub on the request context")
			return
		}
		hub.CaptureMessage("something the handler noticed")
		w.WriteHeader(http.StatusOK)
	})

	server := httptest.NewServer(Sentry()(Observe(discardLogger())(reporting)))
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/disputes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	response.Body.Close()
	sentry.Flush(flushTestTimeout)

	requestID := response.Header.Get("X-Request-Id")
	if requestID == "" {
		t.Fatal("no X-Request-Id on the response")
	}

	tags := sink.tags(t)
	if len(tags) != 1 {
		t.Fatalf("got %d events, want 1: %v", len(tags), tags)
	}
	if tags[0]["request_id"] != requestID {
		t.Errorf("tag request_id = %q, want the header's %q", tags[0]["request_id"], requestID)
	}
}

// A caller that brings its own id keeps it, all the way onto the event.
func TestAnIncomingRequestIDIsTheOneReported(t *testing.T) {
	sink := newSentrySink(t)
	startSentry(t, sink.dsn)

	reporting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentry.GetHubFromContext(r.Context()).CaptureMessage("noticed")
	})
	server := httptest.NewServer(Sentry()(Observe(discardLogger())(reporting)))
	t.Cleanup(server.Close)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("X-Request-Id", "from-the-caller")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	response.Body.Close()
	sentry.Flush(flushTestTimeout)

	tags := sink.tags(t)
	if len(tags) != 1 || tags[0]["request_id"] != "from-the-caller" {
		t.Errorf("tags = %v, want one event tagged from-the-caller", tags)
	}
}

// The panic behaviour is Observe's and stays Observe's, whichever way Sentry is
// configured: one recover, a 500, one log line, and the connection intact.
func TestPanicAnswersTheSameWayWithSentryOnOrOff(t *testing.T) {
	for _, on := range []bool{false, true} {
		name := "sentry off"
		if on {
			name = "sentry on"
		}
		t.Run(name, func(t *testing.T) {
			sink := newSentrySink(t)
			if on {
				startSentry(t, sink.dsn)
			} else {
				startSentry(t, "")
			}

			var logged bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))

			panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				panic("kaboom")
			})
			server := httptest.NewServer(Sentry()(Observe(logger)(panicking)))
			t.Cleanup(server.Close)

			response, err := http.Get(server.URL + "/boom")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()

			if response.StatusCode != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", response.StatusCode)
			}
			if !strings.Contains(string(body), `"error":"internal error"`) {
				t.Errorf("body = %q, want the usual internal error", body)
			}
			if got := strings.Count(logged.String(), `"msg":"handler panicked"`); got != 1 {
				t.Errorf("logged the panic %d times, want exactly 1: %s", got, logged.String())
			}
			if !strings.Contains(logged.String(), `"msg":"request"`) {
				t.Errorf("the request line is missing: %s", logged.String())
			}

			sentry.Flush(flushTestTimeout)
			// With Sentry off nothing is reported at all - the event for a
			// panic is the one the slog bridge makes from the line above, and
			// there is no bridge in this test.
			if n := sink.requests(); n != 0 {
				t.Errorf("sentryhttp reported the panic itself in %d requests; Observe already has it", n)
			}
		})
	}
}

// With no DSN a request never enters any of it: no hub, no wrapped writer, no
// traffic.
func TestDisabledSentryLeavesTheRequestAlone(t *testing.T) {
	sink := newSentrySink(t)
	startSentry(t, "")

	// Over a channel, not a variable: the handler runs on the server's
	// goroutine and the assertions run on this one.
	sawHub := make(chan bool, 1)
	inspect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHub <- sentry.GetHubFromContext(r.Context()) != nil
		w.WriteHeader(http.StatusTeapot)
	})
	server := httptest.NewServer(Sentry()(Observe(discardLogger())(inspect)))
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	response.Body.Close()

	if <-sawHub {
		t.Error("a hub was installed with no DSN configured")
	}
	if response.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want 418", response.StatusCode)
	}
	if response.Header.Get("X-Request-Id") == "" {
		t.Error("no X-Request-Id, so Observe did not run")
	}
	sentry.Flush(flushTestTimeout)
	if n := sink.requests(); n != 0 {
		t.Errorf("the disabled SDK made %d HTTP requests, want 0", n)
	}
}

// Wrapping a ResponseWriter is how the SSE endpoint lost its Flusher once
// already. sentryhttp wraps it a second time, so the unwrap chain is checked
// with Sentry both off and on.
func TestResponseControllerReachesTheRealWriter(t *testing.T) {
	for _, on := range []bool{false, true} {
		name := "sentry off"
		if on {
			name = "sentry on"
		}
		t.Run(name, func(t *testing.T) {
			sink := newSentrySink(t)
			if on {
				startSentry(t, sink.dsn)
			} else {
				startSentry(t, "")
			}

			flushed := make(chan error, 1)
			streaming := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("data: one\n\n"))
				flushed <- http.NewResponseController(w).Flush()
			})
			server := httptest.NewServer(Sentry()(Observe(discardLogger())(streaming)))
			t.Cleanup(server.Close)

			response, err := http.Get(server.URL)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			response.Body.Close()

			if err := <-flushed; err != nil {
				t.Fatalf("streaming is unsupported through the middleware: %v", err)
			}
		})
	}
}

// flushTestTimeout is generous: these tests assert on the absence of traffic as
// often as on its presence, and a flush that gave up early would make the
// absence meaningless.
const flushTestTimeout = 5 * time.Second
