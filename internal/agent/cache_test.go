package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// Without caching the system prompt is a plain string, which is what it has
// always been and what every provider accepts.
func TestSystemIsAStringWhenNothingIsCached(t *testing.T) {
	got := systemFor(Request{System: "be careful"})
	if s, ok := got.(string); !ok || s != "be careful" {
		t.Errorf("systemFor = %#v, want the plain string", got)
	}
}

// With caching it becomes a block, because cache_control lives on a block and
// a bare string has nowhere to put it.
func TestCachingTurnsTheSystemIntoABlock(t *testing.T) {
	encoded, err := json.Marshal(systemFor(Request{System: "be careful", CacheSystem: true}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(encoded)

	for _, want := range []string{`"type":"text"`, `"text":"be careful"`, `"cache_control":{"type":"ephemeral"}`} {
		if !strings.Contains(body, want) {
			t.Errorf("the cached system block is missing %s: %s", want, body)
		}
	}
	if !strings.HasPrefix(body, "[") {
		t.Errorf("the block was not sent as an array: %s", body)
	}
}

func TestAnEmptySystemStaysAbsent(t *testing.T) {
	if got := systemFor(Request{CacheSystem: true}); got != nil {
		t.Errorf("systemFor with no prompt = %#v, want nil", got)
	}
}

// Both calls ask for it, because both have a constant system prompt and a
// constant tool schema in front of a record that changes.
func TestBothCallsAskForTheCache(t *testing.T) {
	gen := &ScriptedCompleter{Responses: []Response{draftResponseFor(t, RecommendRepresent, "x")}}
	if _, err := NewGenerator(gen, "t", Pricing{}, 4096).Write(t.Context(), Facts{}); err != nil {
		t.Fatalf("generator: %v", err)
	}
	if !gen.Requests[0].CacheSystem {
		t.Error("the generator did not ask for its system prompt to be cached")
	}

	ver := &ScriptedCompleter{Responses: []Response{verdictPass(t)}}
	if _, err := NewVerifier(ver, "t", Pricing{}, 2048).Check(t.Context(), Facts{}, "x"); err != nil {
		t.Fatalf("verifier: %v", err)
	}
	if !ver.Requests[0].CacheSystem {
		t.Error("the verifier did not ask for its system prompt to be cached")
	}
}

// A cache read is billed at a fraction of an input token, and getting that
// wrong would report a saving that did not happen - or, as the first version
// did, hide one that did.
func TestCachedTokensAreBilledAtTheirOwnRate(t *testing.T) {
	p := Pricing{
		InputMicrosPerMTok:      3_000_000,
		OutputMicrosPerMTok:     15_000_000,
		CacheWriteMicrosPerMTok: 3_750_000,
		CacheReadMicrosPerMTok:  300_000,
	}

	// A million tokens read from cache costs a tenth of a million fresh ones.
	if got := p.cost(Usage{CacheReadInputTokens: 1_000_000}); got != 300_000 {
		t.Errorf("cache read cost %d, want 300000", got)
	}
	// Writing costs more than reading fresh, which is what makes the first call
	// of a run dearer than the ones after it.
	if got := p.cost(Usage{CacheCreationInputTokens: 1_000_000}); got != 3_750_000 {
		t.Errorf("cache write cost %d, want 3750000", got)
	}
}

// Unset cache rates are derived from the input rate, not set equal to it.
//
// Equal was the first version. It was safe for a ceiling and useless for a
// measurement: a cache working perfectly would have reported no saving at all.
func TestUnsetCacheRatesAreDerived(t *testing.T) {
	p := Pricing{InputMicrosPerMTok: 3_000_000, OutputMicrosPerMTok: 15_000_000}

	if got := p.cost(Usage{CacheReadInputTokens: 1_000_000}); got != 300_000 {
		t.Errorf("derived cache read cost %d, want a tenth of input (300000)", got)
	}
	if got := p.cost(Usage{CacheCreationInputTokens: 1_000_000}); got != 3_750_000 {
		t.Errorf("derived cache write cost %d, want a quarter more than input (3750000)", got)
	}
	// A million fresh tokens still cost what they cost.
	if got := p.cost(Usage{InputTokens: 1_000_000}); got != 3_000_000 {
		t.Errorf("input cost %d", got)
	}
}
