package agent

import (
	"testing"

	"github.com/regisoliveira/dispute-router/internal/llm"
	"github.com/regisoliveira/dispute-router/internal/llm/llmtest"
)

// Both calls ask for it, because both have a constant system prompt and a
// constant tool schema in front of a record that changes.
func TestBothCallsAskForTheCache(t *testing.T) {
	gen := &llmtest.ScriptedCompleter{Responses: []llm.Response{draftResponseFor(t, RecommendRepresent, "x")}}
	if _, err := NewGenerator(gen, "t", llm.Pricing{}, 4096).Write(t.Context(), Facts{}); err != nil {
		t.Fatalf("generator: %v", err)
	}
	if !gen.Requests[0].CacheSystem {
		t.Error("the generator did not ask for its system prompt to be cached")
	}

	ver := &llmtest.ScriptedCompleter{Responses: []llm.Response{verdictPass(t)}}
	if _, err := NewVerifier(ver, "t", llm.Pricing{}, 2048).Check(t.Context(), Facts{}, "x"); err != nil {
		t.Fatalf("verifier: %v", err)
	}
	if !ver.Requests[0].CacheSystem {
		t.Error("the verifier did not ask for its system prompt to be cached")
	}
}
