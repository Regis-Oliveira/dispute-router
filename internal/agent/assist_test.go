package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// What is left here is the one assist test that needs no database at all. Its
// six siblings, which run the whole flow and so write agent_runs, moved to
// assist_integration_test.go behind //go:build integration rather than dragging
// this one behind the tag with them.

// The fingerprint covers the record template, not only the system prompts.
// Every prompt change of a month went into the template and left the old hash
// untouched.
func TestTheFingerprintCoversTheRecordTemplate(t *testing.T) {
	promptsOnly := "sha256:" + hexOf(generatorSystem+"\x00"+verifierSystem)
	if got := promptFingerprint(); got == promptsOnly {
		t.Error("the fingerprint is still the hash of the two system prompts alone")
	}
	first, second := promptFingerprint(), promptFingerprint()
	if first != second {
		t.Error("the fingerprint is not stable across calls")
	}
	template, _ := Facts{}.Render()
	if !strings.Contains(template, "CARDHOLDER CLAIM") || !strings.Contains(template, "PRECEDENT") {
		t.Error("the zero-value record does not render the template headings the fingerprint relies on")
	}
}

func hexOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}
