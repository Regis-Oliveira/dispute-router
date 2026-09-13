package boot

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/getsentry/sentry-go"

	"github.com/regisoliveira/dispute-router/internal/httpx"
)

// There is no Sentry account behind this repository and there does not need to
// be one. A DSN is a URL with a key in front of it, and the SDK's only contact
// with the service is an HTTP POST of an envelope to that URL - so an httptest
// server named by the DSN sees byte for byte what Sentry would have seen.

// sentrySink is that server. It keeps the raw bodies and parses them on the
// test's own goroutine, because a handler goroutine may not call t.Fatal.
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
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	sink.dsn = "http://publickey@" + strings.TrimPrefix(server.URL, "http://") + "/1"
	return sink
}

// sentryEvent is the part of Sentry's event payload these tests assert on.
type sentryEvent struct {
	Message     string                    `json:"message"`
	Level       string                    `json:"level"`
	Logger      string                    `json:"logger"`
	Environment string                    `json:"environment"`
	Fingerprint []string                  `json:"fingerprint"`
	Tags        map[string]string         `json:"tags"`
	Contexts    map[string]map[string]any `json:"contexts"`
	Exception   []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"exception"`
	Request *struct {
		URL         string            `json:"url"`
		Method      string            `json:"method"`
		QueryString string            `json:"query_string"`
		Data        string            `json:"data"`
		Cookies     string            `json:"cookies"`
		Headers     map[string]string `json:"headers"`
		Env         map[string]string `json:"env"`
	} `json:"request"`
}

// requests is how many times the SDK reached the network at all, which is the
// number the disabled case is about.
func (s *sentrySink) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// events parses every envelope received so far and returns the event items.
// An envelope is a header line followed by pairs of item-header and item-body
// lines; anything that is not an event - a transaction, a client report - is
// skipped rather than counted.
func (s *sentrySink) events(t *testing.T) []sentryEvent {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	var events []sentryEvent
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
			var event sentryEvent
			if err := json.Unmarshal(lines[i+1], &event); err != nil {
				t.Fatalf("envelope event body %q: %v", lines[i+1], err)
			}
			events = append(events, event)
		}
	}
	return events
}

// startSentry points the SDK at dsn for the duration of one test. The empty
// string means off, which is what the disabled cases pass.
func startSentry(t *testing.T, dsn string) {
	t.Helper()

	// The SDK falls back to SENTRY_DSN when it is handed an empty one, and a
	// populated .env on the machine running these tests would then quietly
	// turn the disabled case on.
	t.Setenv("SENTRY_DSN", "")

	previous := sentry.CurrentHub().Client()
	t.Cleanup(func() { sentry.CurrentHub().BindClient(previous) })
	sentry.CurrentHub().BindClient(nil)

	if err := Sentry(dsn, "test", "test-release"); err != nil {
		t.Fatalf("Sentry(%q): %v", dsn, err)
	}
}

// testLogger is the wrapper Logger installs, over a buffer instead of stdout.
func testLogger(out io.Writer) *slog.Logger {
	return slog.New(sentryHandler{next: slog.NewJSONHandler(out, logOptions())})
}

// logOptions drops the timestamp so two runs can be compared byte for byte.
func logOptions() *slog.HandlerOptions {
	return &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return attr
		},
	}
}

func TestLoggerWrapsTheHandler(t *testing.T) {
	if _, ok := Logger(true).Handler().(sentryHandler); !ok {
		t.Fatalf("Logger returned a %T, so nothing would be reported", Logger(true).Handler())
	}
}

func TestErrorLogBecomesAnEvent(t *testing.T) {
	sink := newSentrySink(t)
	startSentry(t, sink.dsn)

	var out bytes.Buffer
	logger := testLogger(&out).With("dispute_id", int64(4821))
	logger.ErrorContext(t.Context(), "apply failed",
		"error", errors.New("connection refused"),
		"attempt", 2,
		"action", "refund")

	FlushSentry()

	events := sink.events(t)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	event := events[0]

	if event.Message != "apply failed" {
		t.Errorf("message = %q, want %q", event.Message, "apply failed")
	}
	if event.Level != "error" {
		t.Errorf("level = %q, want error", event.Level)
	}
	if event.Logger != "slog" {
		t.Errorf("logger = %q, want slog", event.Logger)
	}
	if event.Environment != "test" {
		t.Errorf("environment = %q, want test", event.Environment)
	}

	// The With attribute and the record's own, both as tags, both searchable.
	for key, want := range map[string]string{"dispute_id": "4821", "attempt": "2"} {
		if got := event.Tags[key]; got != want {
			t.Errorf("tag %s = %q, want %q", key, got, want)
		}
	}

	// Everything else is on the event as context, not run into the message.
	fields := event.Contexts["log"]
	if got := fields["action"]; got != "refund" {
		t.Errorf("contexts.log.action = %v, want refund", got)
	}
	if got := fields["dispute_id"]; got != float64(4821) {
		t.Errorf("contexts.log.dispute_id = %v, want 4821", got)
	}
	if source, _ := fields["source"].(string); !strings.Contains(source, "sentry_test.go:") {
		t.Errorf("contexts.log.source = %v, want this file and a line", fields["source"])
	}

	if len(event.Exception) != 1 || event.Exception[0].Value != "connection refused" {
		t.Errorf("exception = %+v, want the logged error", event.Exception)
	}
	// One issue per operation, not one per dispute id in the error text.
	if len(event.Fingerprint) != 1 || event.Fingerprint[0] != "apply failed" {
		t.Errorf("fingerprint = %v, want [apply failed]", event.Fingerprint)
	}
}

func TestBelowErrorIsNotAnEvent(t *testing.T) {
	sink := newSentrySink(t)
	startSentry(t, sink.dsn)

	var out bytes.Buffer
	logger := testLogger(&out)
	logger.InfoContext(t.Context(), "decided", "dispute_id", int64(7))
	logger.WarnContext(t.Context(), "lock had already expired when releasing")

	FlushSentry()

	if n := sink.requests(); n != 0 {
		t.Fatalf("info and warn reached Sentry in %d requests, want 0", n)
	}
	// They are still logged, which is the whole point of a bridge rather than
	// a replacement handler.
	if !strings.Contains(out.String(), `"msg":"decided"`) {
		t.Fatalf("the info line was not logged: %s", out.String())
	}
}

// The one that matters most: with no DSN nothing is initialised, nothing is
// sent, and the log output is byte for byte what the handler underneath
// produces on its own.
func TestEmptyDSNSendsNothingAndChangesNothing(t *testing.T) {
	sink := newSentrySink(t)
	startSentry(t, "")

	if client := sentry.CurrentHub().Client(); client != nil {
		t.Fatalf("an empty DSN left a client bound: %v", client)
	}

	write := func(logger *slog.Logger) {
		logger = logger.With("dispute_id", int64(4821))
		logger.InfoContext(t.Context(), "decided", "action", "refund")
		logger.ErrorContext(t.Context(), "apply failed", "error", errors.New("connection refused"))
	}

	var wrapped, control bytes.Buffer
	write(testLogger(&wrapped))
	write(slog.New(slog.NewJSONHandler(&control, logOptions())))

	FlushSentry()

	if n := sink.requests(); n != 0 {
		t.Errorf("the disabled SDK made %d HTTP requests, want 0", n)
	}
	if wrapped.String() != control.String() {
		t.Errorf("the logs changed with Sentry off:\n got %s\nwant %s", wrapped.String(), control.String())
	}
}

func TestMalformedDSNIsAStartupError(t *testing.T) {
	t.Setenv("SENTRY_DSN", "")
	previous := sentry.CurrentHub().Client()
	t.Cleanup(func() { sentry.CurrentHub().BindClient(previous) })

	if err := Sentry("not-a-dsn", "test", ""); err == nil {
		t.Fatal("Sentry accepted a malformed DSN instead of refusing to start")
	}
}

// The composition test: a panicking handler behind the two middlewares, wired
// the way every main wires them. One event, carrying the request id the
// response header reports, and a 500 that is unchanged.
func TestPanickingHandlerReportsWithItsRequestID(t *testing.T) {
	sink := newSentrySink(t)
	startSentry(t, sink.dsn)

	var out bytes.Buffer
	logger := testLogger(&out)

	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	})
	server := httptest.NewServer(httpx.Sentry()(httpx.Observe(logger)(panicking)))
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/disputes?state=open")
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
	requestID := response.Header.Get("X-Request-Id")
	if requestID == "" {
		t.Fatal("no X-Request-Id on the response")
	}
	if !strings.Contains(out.String(), `"msg":"handler panicked"`) {
		t.Errorf("the panic was not logged: %s", out.String())
	}

	FlushSentry()

	events := sink.events(t)
	if len(events) != 1 {
		t.Fatalf("got %d events, want exactly 1 - two recovers would make two: %+v", len(events), events)
	}
	event := events[0]

	if event.Message != "handler panicked" {
		t.Errorf("message = %q, want %q", event.Message, "handler panicked")
	}
	if event.Tags["request_id"] != requestID {
		t.Errorf("tag request_id = %q, want the header's %q", event.Tags["request_id"], requestID)
	}
	if got := event.Contexts["log"]["panic"]; got != "kaboom" {
		t.Errorf("contexts.log.panic = %v, want kaboom", got)
	}

	// And nothing was taken off the request but its method and path.
	if event.Request == nil {
		t.Fatal("no request on the event, so the hub was never installed")
	}
	if event.Request.Method != http.MethodGet || !strings.HasSuffix(event.Request.URL, "/disputes") {
		t.Errorf("request = %s %s, want GET .../disputes", event.Request.Method, event.Request.URL)
	}
	if len(event.Request.Headers) != 0 || len(event.Request.Env) != 0 ||
		event.Request.QueryString != "" || event.Request.Data != "" || event.Request.Cookies != "" {
		t.Errorf("the event carried request data it was configured not to collect: %+v", *event.Request)
	}
}
