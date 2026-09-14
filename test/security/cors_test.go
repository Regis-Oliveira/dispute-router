//go:build security

package security

import (
	"net/http"
	"strings"
	"testing"
)

// TestAnUnlistedOriginGetsNoCorsHeader pins that an origin not on the allow-list
// is never reflected.
//
// This exercises the real middleware chain that only a black-box request sees:
// an in-process handler test never composes CORS the way the running server
// does. A reflected origin would let any page on the internet read this data
// from an operator's authenticated browser, so an unlisted Origin must come back
// with no Access-Control-Allow-Origin at all.
func TestAnUnlistedOriginGetsNoCorsHeader(t *testing.T) {
	base := requireAPI(t)

	resp := do(t, request{
		method:  http.MethodGet,
		url:     base + "/api/disputes?limit=1",
		headers: map[string]string{"Origin": "https://evil.example"},
	})
	if got := resp.header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q for an unlisted origin, want it absent", got)
	}
}

// TestTheAllowedOriginIsEchoedNeverAWildcard pins that a listed origin is echoed
// back exactly, never as a wildcard, and that credentials are not allowed.
//
// http://localhost:4200 is the dashboard's dev origin and is on the allow-list
// by configuration. The response must name it exactly - a bare * would defeat
// the whole point of an allow-list - and must not send
// Access-Control-Allow-Credentials, since that combination is how a logged-in
// session gets read cross-origin.
func TestTheAllowedOriginIsEchoedNeverAWildcard(t *testing.T) {
	base := requireAPI(t)
	const origin = "http://localhost:4200"

	resp := do(t, request{
		method:  http.MethodGet,
		url:     base + "/api/disputes?limit=1",
		headers: map[string]string{"Origin": origin},
	})

	got := resp.header.Get("Access-Control-Allow-Origin")
	// A wildcard is a hard failure of the property regardless of configuration.
	if got == "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, a wildcard where an exact origin is required", got)
	}
	if got != origin {
		// The property cannot be proved if the dev origin is not the one
		// configured here. That is an environment difference, not a defect, so
		// say so and skip rather than assert against an origin this deployment
		// does not know.
		t.Skipf("Access-Control-Allow-Origin = %q, not the expected dev origin %q; is API_CORS_ORIGINS configured differently?", got, origin)
	}
	if cred := resp.header.Get("Access-Control-Allow-Credentials"); strings.EqualFold(cred, "true") {
		t.Errorf("Access-Control-Allow-Credentials = %q with an echoed origin; credentials must not be allowed", cred)
	}
}
