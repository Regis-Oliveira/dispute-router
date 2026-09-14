//go:build security

package security

import (
	"fmt"
	"net/http"
	"os"
	"testing"
)

// TestAForgedForwardedForDoesNotEarnAFreshBucket pins that the ingest rate
// limiter keys on the real peer address and ignores a client-supplied
// X-Forwarded-For.
//
// Gated behind ATTACK_RATE=1 because it deliberately drains a real Redis token
// bucket and fires hundreds of requests: slow, and stateful in a way a plain
// `make attack` should not trigger on every run. When it does run, it sends far
// more than the burst, with a different forged X-Forwarded-For each time. If the
// limiter trusted that header, each forged value would get its own fresh bucket
// and no request would ever be limited; because it keys on the loopback peer,
// the bucket drains and a 429 appears. One 429 is the proof, so the loop stops
// at the first.
func TestAForgedForwardedForDoesNotEarnAFreshBucket(t *testing.T) {
	if os.Getenv("ATTACK_RATE") != "1" {
		t.Skip("set ATTACK_RATE=1 to run the rate-limit attack; it drains a real Redis bucket and is slow")
	}
	base := requireIngest(t)

	// Comfortably above the default IP burst (240) so the bucket is exhausted
	// even accounting for the slow refill during the loop. The requests are
	// unsigned, which is enough: the IP limit is the first step in the pipeline,
	// spent before the signature is ever looked at.
	const attempts = 800
	body := []byte(`{"type":"dispute.opened","data":{"merchant_id":"mrc_forged"}}`)

	saw429 := false
	for i := range attempts {
		resp := do(t, request{
			method: http.MethodPost,
			url:    base + "/webhooks/processor",
			headers: map[string]string{
				"Content-Type": "application/json",
				// A different forged origin every time. If this earned a fresh
				// bucket, 429 would never appear.
				"X-Forwarded-For": fmt.Sprintf("203.0.113.%d", i%256),
			},
			body: body,
		})
		if resp.status == http.StatusTooManyRequests {
			saw429 = true
			break
		}
	}
	if !saw429 {
		t.Errorf("no 429 in %d requests with a rotating X-Forwarded-For: the limiter may be keying on the forged header",
			attempts)
	}
}
