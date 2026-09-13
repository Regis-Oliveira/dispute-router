package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func corsHandler() http.Handler {
	return CORS([]string{"http://localhost:4200"})(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)
}

// Every method the API actually routes has to be advertised, or the browser
// refuses the request before it is ever sent. The presigned-upload endpoint is
// a POST, and it was missing from this list.
func TestPreflightAdvertisesEveryRoutedMethod(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodOptions, "/api/disputes/1/evidence", nil)
	request.Header.Set("Origin", "http://localhost:4200")
	request.Header.Set("Access-Control-Request-Method", "POST")

	recorder := httptest.NewRecorder()
	corsHandler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", recorder.Code)
	}

	allowed := recorder.Header().Get("Access-Control-Allow-Methods")
	for _, method := range []string{"GET", "POST", "OPTIONS"} {
		if !strings.Contains(allowed, method) {
			t.Errorf("Allow-Methods = %q, missing %s", allowed, method)
		}
	}
}

// A reflected origin would let any page on the internet read this data out of a
// logged-in operator's browser.
func TestUnknownOriginGetsNoAllowHeader(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "/api/disputes", nil)
	request.Header.Set("Origin", "https://evil.example")

	recorder := httptest.NewRecorder()
	corsHandler().ServeHTTP(recorder, request)

	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want empty for an unlisted origin", got)
	}
}

func TestAllowedOriginIsEchoedExactly(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "/api/disputes", nil)
	request.Header.Set("Origin", "http://localhost:4200")

	recorder := httptest.NewRecorder()
	corsHandler().ServeHTTP(recorder, request)

	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:4200" {
		t.Errorf("Allow-Origin = %q", got)
	}
	// Without Vary, a cache can serve one origin's response to another.
	if got := recorder.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary = %q, want it to include Origin", got)
	}
}

// A wildcard would defeat the whole point of the allowlist.
func TestNoWildcardIsEverSent(t *testing.T) {
	t.Parallel()

	for _, origin := range []string{"http://localhost:4200", "https://evil.example", ""} {
		request := httptest.NewRequest(http.MethodGet, "/api/disputes", nil)
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		recorder := httptest.NewRecorder()
		corsHandler().ServeHTTP(recorder, request)

		if recorder.Header().Get("Access-Control-Allow-Origin") == "*" {
			t.Errorf("origin %q produced a wildcard Allow-Origin", origin)
		}
	}
}
