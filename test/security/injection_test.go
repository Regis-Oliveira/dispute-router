//go:build security

package security

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestASearchTermIsNeverInterpolatedIntoSql is the regression guard on query
// parameterisation, proved from the outside.
//
// Every filter value reaches Postgres as a bind parameter. A classic injection
// in the merchant filter - ' OR '1'='1 and friends - must never widen the result
// set and must never provoke a 500. Bound as a value, the payload is just a
// merchant id that matches nothing, so the page comes back smaller than the
// unfiltered total. Interpolated into SQL, the OR would return the whole table
// or error; this catches both. This defense must STAY working - a failure here
// is a real regression, not a pinned gap.
func TestASearchTermIsNeverInterpolatedIntoSql(t *testing.T) {
	base := requireAPI(t)

	var all disputePage
	getJSON(t, base+"/api/disputes?limit=1", &all)
	baseline := all.Page.Total
	if baseline == 0 {
		t.Skip("no disputes seeded; the full-dump check needs a non-empty table")
	}

	payloads := []string{
		`' OR '1'='1`,
		`'; DROP TABLE disputes;--`,
		`" OR ""="`,
		`1) OR (1=1`,
		`\' OR 1=1 --`,
		`%27 OR 1=1`,
	}
	for _, p := range payloads {
		t.Run(p, func(t *testing.T) {
			u := base + "/api/disputes?merchant=" + url.QueryEscape(p)
			resp := do(t, request{method: http.MethodGet, url: u})

			// A 500 is the failure that matters most: it means the payload
			// reached the database as something other than a bound value.
			if resp.status == http.StatusInternalServerError {
				t.Fatalf("payload provoked a 500: %s", resp.body)
			}
			if resp.status != http.StatusOK && resp.status != http.StatusBadRequest {
				t.Fatalf("unexpected status %d; want 200 or 400; body %s", resp.status, resp.body)
			}
			if resp.status == http.StatusBadRequest {
				return // Refused outright is fine; nothing was interpreted.
			}

			var got disputePage
			if err := json.Unmarshal(resp.body, &got); err != nil {
				t.Fatalf("decode: %v; body %s", err, resp.body)
			}
			// The payload is a merchant id that exists nowhere, so a correctly
			// parameterised query returns strictly fewer rows than no filter at
			// all. A total equal to the baseline would mean the filter was
			// ignored or the OR took effect - a full unfiltered dump.
			if got.Page.Total >= baseline {
				t.Errorf("filtered total %d is not below the unfiltered %d: the term was not applied as a bound value",
					got.Page.Total, baseline)
			}
		})
	}
}

// TestAFilterOutsideItsWhitelistIsRefused pins that a filter value the schema
// cannot hold is a 400, not a silently-empty page.
//
// A quietly-ignored bad filter shows an operator more data than they asked to
// see; an out-of-whitelist sort key would be an injection surface if it were not
// refused. Each of these must come back 400. This defense must STAY working.
//
// The parameter names here are the API's own, confirmed against the live
// service: the card-network filter is `network` (not `card_network`) and the
// date filter is `opened_from` (not `opened_after`). The other spellings are
// simply unknown query keys the API ignores - asserting 400 on those would be
// testing a filter that does not exist.
func TestAFilterOutsideItsWhitelistIsRefused(t *testing.T) {
	base := requireAPI(t)

	// Built through url.Values so each value is encoded: a raw ';' in a query
	// string is dropped by the server's parser before the handler sees it, which
	// would test nothing. Encoded, "id;DROP" reaches the sort whitelist as a
	// value and is refused - which is the point.
	for _, tc := range []struct{ key, value string }{
		{"state", "nonsense"},
		{"sort", "id;DROP"},
		{"network", "maestro"},
		{"min_amount", "-1"},
		{"opened_from", "notadate"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			q := url.Values{tc.key: {tc.value}}
			resp := do(t, request{method: http.MethodGet, url: base + "/api/disputes?" + q.Encode()})
			if resp.status != http.StatusBadRequest {
				t.Errorf("%s=%s: status %d, want 400; body %s", tc.key, tc.value, resp.status, resp.body)
			}
		})
	}
}

// TestALimitAboveTheCapIsClampedNotHonored pins the paging bounds: a huge limit
// is clamped to the cap, a non-positive limit is refused, and paging past the
// deep-paging cap is refused.
//
// The clamp is what stops one request from asking the database for the whole
// table; the refusals stop a caller getting a page size or an offset the API
// never promised. Clamped-not-defaulted matters too: limit=100000 must return
// the cap's worth of rows, never quietly fall back to the default page size.
func TestALimitAboveTheCapIsClampedNotHonored(t *testing.T) {
	base := requireAPI(t)
	const maxRows = 200

	t.Run("a limit above the cap returns at most the cap", func(t *testing.T) {
		var page disputePage
		getJSON(t, base+"/api/disputes?limit=100000", &page)
		if len(page.Rows) > maxRows {
			t.Errorf("got %d rows, want at most %d", len(page.Rows), maxRows)
		}
		// The echoed page size proves the clamp, not just that the table happened
		// to hold fewer than the cap.
		if page.Page.Limit != maxRows {
			t.Errorf("page.limit = %d, want it clamped to %d", page.Page.Limit, maxRows)
		}
	})

	for _, q := range []string{"limit=0", "limit=-1", "offset=10001"} {
		t.Run(q+" is refused", func(t *testing.T) {
			resp := do(t, request{method: http.MethodGet, url: base + "/api/disputes?" + q})
			if resp.status != http.StatusBadRequest {
				t.Errorf("%q: status %d, want 400; body %s", q, resp.status, resp.body)
			}
		})
	}
}

// TestAnEvidenceFilenameCannotEscapeItsPrefix pins that a caller-supplied
// filename cannot place an object outside its dispute's own key prefix.
//
// The S3 key is also a URL, and a filename is attacker-controlled: a traversal
// that survived into the key would let evidence for one dispute be written over
// another, or outside the disputes/ tree entirely. Whatever the filename, the
// minted key must stay under disputes/{id}/ with no path separator left in the
// name part. This defense must STAY working.
func TestAnEvidenceFilenameCannotEscapeItsPrefix(t *testing.T) {
	base := requireAPI(t)
	id := firstDisputeID(t, base)
	wantPrefix := fmt.Sprintf("disputes/%d/", id)

	for _, filename := range []string{
		"../../../etc/passwd",
		"..%2f..%2fx",
		"/etc/passwd",
		"receipt\n.pdf",
	} {
		t.Run(filename, func(t *testing.T) {
			// Marshalled so the newline is escaped in transit rather than
			// breaking the JSON body.
			body, err := json.Marshal(map[string]string{
				"filename":     filename,
				"content_type": "application/pdf",
			})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			resp := do(t, request{
				method:  http.MethodPost,
				url:     fmt.Sprintf("%s/api/disputes/%d/evidence", base, id),
				headers: map[string]string{"Content-Type": "application/json"},
				body:    body,
			})
			if resp.status != http.StatusOK {
				t.Fatalf("presign: status %d, want 200; body %s", resp.status, resp.body)
			}

			var target struct {
				Key    string            `json:"key"`
				Fields map[string]string `json:"fields"`
			}
			if err := json.Unmarshal(resp.body, &target); err != nil {
				t.Fatalf("decode target: %v; body %s", err, resp.body)
			}

			// Both the key the client is told and the key the signed policy
			// enforces (fields["key"]) must land under this dispute's prefix.
			for label, key := range map[string]string{"key": target.Key, `fields["key"]`: target.Fields["key"]} {
				if key == "" {
					continue
				}
				if !strings.HasPrefix(key, wantPrefix) {
					t.Errorf("%s = %q, want it under %q", label, key, wantPrefix)
					continue
				}
				// Everything after the prefix must be one segment: a surviving
				// '/' would be a path the traversal built.
				if name := strings.TrimPrefix(key, wantPrefix); strings.Contains(name, "/") {
					t.Errorf("%s = %q has a path separator in the name part %q; traversal was not stripped",
						label, key, name)
				}
			}
		})
	}
}
