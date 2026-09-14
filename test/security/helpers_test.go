//go:build security

package security

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// httpClient is the single client the whole suite talks through. The timeout is
// short on purpose: a target that hangs should fail a test in seconds rather
// than stall the run, and nothing here is a long-lived stream.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// envOr reads a base URL from the environment, falling back to the localhost
// default. The suite is aimed at a running stack, not compiled against one, so
// the targets are configuration - overridable to point the same tests at a
// deployed instance without recompiling.
func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func apiBase() string { return envOr("ATTACK_API_URL", "http://localhost:8081") }

// ingestBase is a distinct name from the simulator's INGEST_URL, which is the
// full webhook endpoint (…/webhooks/processor) rather than a service base, and
// which .env sets - reusing it made the health check probe the wrong path.
func ingestBase() string { return envOr("ATTACK_INGEST_URL", "http://localhost:8080") }

// requireAPI returns the read API's base URL, skipping the test unless the
// service answers its health check.
func requireAPI(t *testing.T) string {
	t.Helper()
	base := apiBase()
	requireHealthy(t, base, "read API", "ATTACK_API_URL")
	return base
}

// requireIngest returns the ingest service's base URL on the same terms.
func requireIngest(t *testing.T) string {
	t.Helper()
	base := ingestBase()
	requireHealthy(t, base, "ingest service", "ATTACK_INGEST_URL")
	return base
}

// requireHealthy skips the test unless base answers 200 on /healthz.
//
// A black-box suite must not fail merely because a service is down: the repo's
// live tests skip on an unset DATABASE_URL, and this is the same contract for a
// target reached over the network rather than a datastore reached by DSN.
func requireHealthy(t *testing.T, base, name, envKey string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/healthz", nil)
	if err != nil {
		t.Fatalf("build health request for %s: %v", name, err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Skipf("%s unreachable at %s (start it, or set %s): %v", name, base, envKey, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("%s at %s is not healthy (status %d); skipping", name, base, resp.StatusCode)
	}
}

// request is one HTTP call described by its parts. headers and body are
// optional; a nil body sends none.
type request struct {
	method  string
	url     string
	headers map[string]string
	body    []byte
}

// response is what came back: the status, the whole body, and the headers the
// CORS tests read.
type response struct {
	status int
	body   []byte
	header http.Header
}

// do issues a request and reads the whole response. A transport error fails the
// test, named by method and URL so the failure says what could not be reached -
// a build error, not a security finding.
func do(t *testing.T, r request) response {
	t.Helper()
	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(t.Context(), r.method, r.url, body)
	if err != nil {
		t.Fatalf("build %s %s: %v", r.method, r.url, err)
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", r.method, r.url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s %s: %v", r.method, r.url, err)
	}
	return response{status: resp.StatusCode, body: raw, header: resp.Header}
}

// getJSON GETs a URL, requires 200, and decodes the body into v.
//
// The 200 check lives in the helper because it is itself an assertion the
// callers rely on: a read that started answering 401 - an auth layer arriving in
// front of the read API - would fail here loudly rather than decode an empty
// body and pass.
func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp := do(t, request{method: http.MethodGet, url: url})
	if resp.status != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200; body %s", url, resp.status, resp.body)
	}
	if err := json.Unmarshal(resp.body, v); err != nil {
		t.Fatalf("decode %s: %v; body %s", url, err, resp.body)
	}
}

// disputePage is the shape of GET /api/disputes: a page of rows plus its total.
// Only the fields the suite reads are declared.
type disputePage struct {
	Rows []struct {
		ID int64 `json:"id"`
	} `json:"rows"`
	Page struct {
		Limit int   `json:"limit"`
		Total int64 `json:"total"`
	} `json:"page"`
}

// firstDisputeID discovers a real dispute id at runtime.
//
// The seed is regenerated between runs, so an id is never hard-coded; a query
// for a single row is the cheapest way to learn one that exists. Skips when
// nothing is seeded, the way the MCP tests skip rather than fail on an empty
// dataset.
func firstDisputeID(t *testing.T, base string) int64 {
	t.Helper()
	var page disputePage
	getJSON(t, base+"/api/disputes?limit=1", &page)
	if len(page.Rows) == 0 {
		t.Skip("no disputes seeded; run make seed")
	}
	return page.Rows[0].ID
}
