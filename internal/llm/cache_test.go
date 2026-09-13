package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// Without caching the system prompt is a plain string, which is what it has
// always been and what every provider accepts.
func TestSystemIsAStringWhenNothingIsCached(t *testing.T) {
	t.Parallel()

	got := systemFor(Request{System: "be careful"})
	if s, ok := got.(string); !ok || s != "be careful" {
		t.Errorf("systemFor = %#v, want the plain string", got)
	}
}

// With caching it becomes a block, because cache_control lives on a block and
// a bare string has nowhere to put it.
func TestCachingTurnsTheSystemIntoABlock(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

	if got := systemFor(Request{CacheSystem: true}); got != nil {
		t.Errorf("systemFor with no prompt = %#v, want nil", got)
	}
}
