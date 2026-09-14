//go:build security

package security

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestAnUnsignedWebhookIsRejected pins that the ingest endpoint refuses a
// delivery with no signature, and that its refusal names nothing.
//
// No valid signature is needed to prove this: the header check runs before
// anything in the body is acted on. The refusal is a 401 and its body must be
// generic - the same answer an unknown merchant gets - because a reply that
// named the merchant would turn the endpoint into an oracle for enumerating
// merchant ids. The signature pipeline itself is covered in-process by
// internal/ingest/handler_test.go; this only pins the black-box contract of the
// unsigned case.
func TestAnUnsignedWebhookIsRejected(t *testing.T) {
	base := requireIngest(t)

	const merchant = "mrc_havenroast"
	body := fmt.Sprintf(`{"type":"dispute.opened","data":{"merchant_id":%q}}`, merchant)
	resp := do(t, request{
		method:  http.MethodPost,
		url:     base + "/webhooks/processor",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    []byte(body),
	})
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook: status %d, want 401; body %s", resp.status, resp.body)
	}

	// The refusal must not reveal whether that merchant exists.
	lower := strings.ToLower(string(resp.body))
	if strings.Contains(lower, merchant) || strings.Contains(lower, "merchant") {
		t.Errorf("rejection body leaks merchant detail, an enumeration oracle: %s", resp.body)
	}
}

// TestAnOversizedBodyIsRejectedBeforeAnythingElse pins that a body over the cap
// is refused with 413, and that the cap is enforced ahead of the signature.
//
// The request carries no signature. If the size limit were checked after
// authentication, an oversized unsigned body would come back 401; a 413 proves
// the body cap sits at the very front of the pipeline, where it belongs - the
// point of a size limit is to fail before spending any work on the request.
func TestAnOversizedBodyIsRejectedBeforeAnythingElse(t *testing.T) {
	base := requireIngest(t)

	// The cap is 64KB (INGEST_MAX_BODY_BYTES). A comfortable margin over it keeps
	// the test robust to a differently-configured cap of the same order.
	oversized := bytes.Repeat([]byte("x"), 100*1024)
	resp := do(t, request{
		method:  http.MethodPost,
		url:     base + "/webhooks/processor",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    oversized,
	})
	if resp.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status %d, want 413; body %s", resp.status, resp.body)
	}
}
