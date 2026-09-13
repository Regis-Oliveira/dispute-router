package llm

import (
	"encoding/json"
	"testing"
)

// A block that arrived from the API goes back byte for byte, whatever shape it
// had: an empty thinking string, a field this code has never heard of. The
// first real multi-turn run failed on exactly this - the echo had dropped a
// field the API requires.
func TestBlocksFromTheAPIAreEchoedVerbatim(t *testing.T) {
	wire := `{"type":"thinking","thinking":"","signature":"sig-1","future_field":{"x":1}}`
	var block ContentBlock
	if err := json.Unmarshal([]byte(wire), &block); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	back, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(back) != wire {
		t.Errorf("block changed on the way round:\n got %s\nwant %s", back, wire)
	}
	// And a block this code builds still marshals from its fields.
	ours, _ := json.Marshal(ContentBlock{Type: "tool_result", ToolUseID: "t1", Content: "ok"})
	if string(ours) != `{"type":"tool_result","tool_use_id":"t1","content":"ok"}` {
		t.Errorf("a locally built block marshals wrongly: %s", ours)
	}
}

// The raw form has to survive a trip through a slice inside a message, which is
// how every echoed block actually travels. It is the asymmetric receivers that
// make that work - a pointer UnmarshalJSON to store the bytes, a value
// MarshalJSON so the method is found on a []ContentBlock element - and getting
// either wrong is silent: the fields still encode, and only the API complains.
func TestTheRawFormSurvivesInsideAMessage(t *testing.T) {
	wire := `{"content":[{"type":"thinking","thinking":"","signature":"sig-1","future_field":{"x":1}}],"stop_reason":"tool_use","usage":{}}`

	var response Response
	if err := json.Unmarshal([]byte(wire), &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	back, err := json.Marshal(Message{Role: "assistant", Content: response.Content})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig-1","future_field":{"x":1}}]}`
	if string(back) != want {
		t.Errorf("the echoed message lost the block's raw form:\n got %s\nwant %s", back, want)
	}
}
