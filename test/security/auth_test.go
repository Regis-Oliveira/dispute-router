//go:build security

package security

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// TestTheReadApiNeedsNoCredentials pins that the read API authenticates nothing.
//
// This is not a claim that unauthenticated reads are correct - they are the
// known gap. GET /api/disputes with no Authorization, cookie or token returns
// 200 and real rows, which means anything that can reach the port can pull the
// whole dispute book. The assertion is today's behaviour: the day an identity is
// put in front of this endpoint it will answer 401 here, getJSON will fail, and
// the change is caught. A green run is the claim that the door is still open.
func TestTheReadApiNeedsNoCredentials(t *testing.T) {
	base := requireAPI(t)

	// getJSON requires 200; that is the first half of the property. No auth
	// header is sent at all.
	var page disputePage
	getJSON(t, base+"/api/disputes", &page)

	if len(page.Rows) == 0 {
		t.Skip("read API returned no rows; nothing to exfiltrate, run make seed")
	}
	// A real id is the second half: proof the body is live data, not an empty
	// envelope a future auth layer might still return with a 200.
	if page.Rows[0].ID == 0 {
		t.Errorf("unauthenticated read returned a row with no id; expected real dispute data")
	}
}

// TestTheDecisionEndpointTakesAnyCallersWord pins that the one write in the read
// service has no authentication in front of it.
//
// The known gap, stated plainly: reviewed_by is whatever the caller typed, and
// nothing verifies who the caller is. To prove that without submitting a real
// dispute to a card network, the request targets a run id that does not exist. A
// 404/409 means the request sailed past where an auth gate would sit and was
// turned away only by business logic (no such run, or already decided) - never
// by a 401. A change that adds authentication would answer 401/403 here first,
// and this test would fail, which is the point.
func TestTheDecisionEndpointTakesAnyCallersWord(t *testing.T) {
	base := requireAPI(t)

	// A deliberately out-of-range id: large enough that the seed will never
	// reach it, so this can never mutate a real review. The body is a valid,
	// well-formed decision precisely so the request is rejected by identity, not
	// by shape - a well-formed body is exactly what an auth gate would stop.
	const nonexistentRun = 999999999999
	resp := do(t, request{
		method:  http.MethodPost,
		url:     fmt.Sprintf("%s/api/reviews/%d/decision", base, nonexistentRun),
		headers: map[string]string{"Content-Type": "application/json"},
		body:    []byte(`{"decision":"submitted","reviewer":"anybody"}`),
	})

	switch resp.status {
	case http.StatusUnauthorized, http.StatusForbidden:
		t.Errorf("decision endpoint answered %d: an auth gate now guards it. "+
			"If that is intended, this pinned-gap test has done its job and should be "+
			"rewritten to assert that auth is required.", resp.status)
	case http.StatusNotFound, http.StatusConflict:
		// Reached the store with no credentials and was refused only because the
		// run does not exist. That is the gap this test exists to record.
	case http.StatusNoContent:
		t.Fatalf("a non-existent run reported a successful decision (204); " +
			"the id guard is broken and this may have mutated real data")
	default:
		t.Errorf("unexpected status %d from an unauthenticated decision; body %s", resp.status, resp.body)
	}
}

// TestThePresignEndpointMintsAnUploadCredentialUnauthenticated pins that an
// unauthenticated caller can obtain a write credential for the evidence bucket.
//
// The known gap: no identity is required to mint a presigned upload target, so
// anyone who can reach the port can write objects into the evidence bucket under
// a real dispute's prefix. The assertion is today's behaviour; adding auth
// should make this fail with a 401 instead of a signed target, not pass quietly.
func TestThePresignEndpointMintsAnUploadCredentialUnauthenticated(t *testing.T) {
	base := requireAPI(t)
	id := firstDisputeID(t, base)

	resp := do(t, request{
		method:  http.MethodPost,
		url:     fmt.Sprintf("%s/api/disputes/%d/evidence", base, id),
		headers: map[string]string{"Content-Type": "application/json"},
		body:    []byte(`{"filename":"receipt.pdf","content_type":"application/pdf"}`),
	})
	if resp.status != http.StatusOK {
		t.Fatalf("presign without auth: status %d, want 200; body %s", resp.status, resp.body)
	}

	var target struct {
		Key    string            `json:"key"`
		URL    string            `json:"url"`
		Fields map[string]string `json:"fields"`
	}
	if err := json.Unmarshal(resp.body, &target); err != nil {
		t.Fatalf("decode presign target: %v; body %s", err, resp.body)
	}
	// A usable credential is url + key + the signed form fields. Anything less
	// would not actually let the caller write, and the point is that it does.
	if target.URL == "" || target.Key == "" || len(target.Fields) == 0 {
		t.Errorf("expected a usable upload credential (url, key, fields); got %+v", target)
	}
}
