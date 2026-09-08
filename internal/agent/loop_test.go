package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func testLoop(t *testing.T, script *ScriptedCompleter, budget Budget) *Loop {
	t.Helper()
	if budget.MaxTokens == 0 {
		budget.MaxTokens = 2048
	}
	return NewLoop(script, offlineRegistry(), "test-model", Pricing{
		InputMicrosPerMTok:  3_000_000, // $3 / Mtok
		OutputMicrosPerMTok: 15_000_000,
	}, budget)
}

// The tool calls below deliberately fail inside the registry - an unknown name,
// a misspelled argument - because those are the paths that produce a real
// tool_result without touching the database. The loop does not care what a tool
// did, only that it got something back, so this exercises it fully.
func badCall(id string) ToolUse {
	return ToolUse{ID: id, Name: "list_disputes", Input: json.RawMessage(`{"nope":1}`)}
}

func TestAnAnswerWithNoToolsFinishes(t *testing.T) {
	script := &ScriptedCompleter{Responses: []Response{
		Says("Nothing is due today.", Usage{InputTokens: 1200, OutputTokens: 40}),
	}}

	result, err := testLoop(t, script, Budget{MaxTurns: 8, MaxCostMicros: 100_000}).
		Run(context.Background(), "system", "what is due today?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !result.Halt.Done() {
		t.Errorf("halt = %q, want completed", result.Halt)
	}
	if result.Text != "Nothing is due today." {
		t.Errorf("text = %q", result.Text)
	}
	if len(result.Turns) != 1 || script.Calls() != 1 {
		t.Errorf("%d turns over %d calls, want 1 and 1", len(result.Turns), script.Calls())
	}
	// 1200 in at $3/Mtok = 3600 micros; 40 out at $15/Mtok = 600 micros.
	if result.CostMicros != 4200 {
		t.Errorf("cost = %d micros, want 4200", result.CostMicros)
	}
}

// The round trip is the loop: the model asks, the tools answer, and the answer
// goes back attached to the id it belongs to.
func TestToolResultsGoBackOnTheNextTurn(t *testing.T) {
	script := &ScriptedCompleter{Responses: []Response{
		Calls(Usage{InputTokens: 1000, OutputTokens: 50}, badCall("toolu_a")),
		Says("Done.", Usage{InputTokens: 1400, OutputTokens: 30}),
	}}

	result, err := testLoop(t, script, Budget{MaxTurns: 8, MaxCostMicros: 1_000_000}).
		Run(context.Background(), "system", "look something up")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Halt.Done() {
		t.Fatalf("halt = %q, want completed", result.Halt)
	}

	if len(script.Requests) != 2 {
		t.Fatalf("%d requests, want 2", len(script.Requests))
	}
	sent := script.Requests[1].Messages
	if len(sent) != 3 {
		t.Fatalf("second request carried %d messages, want user + assistant + tool results", len(sent))
	}
	if sent[1].Role != "assistant" {
		t.Errorf("message 1 role = %q, want assistant", sent[1].Role)
	}
	results := sent[2]
	if results.Role != "user" {
		t.Errorf("tool results were sent as role %q, want user", results.Role)
	}
	if len(results.Content) != 1 || results.Content[0].Type != "tool_result" {
		t.Fatalf("expected one tool_result block, got %+v", results.Content)
	}
	if results.Content[0].ToolUseID != "toolu_a" {
		t.Errorf("tool_use_id = %q, want toolu_a; the model cannot match it otherwise",
			results.Content[0].ToolUseID)
	}

	// The trace has to name what was called, or the audit trail is a token count.
	if len(result.Turns[0].ToolCalls) != 1 || result.Turns[0].ToolCalls[0].Name != "list_disputes" {
		t.Errorf("turn 0 tool calls = %+v", result.Turns[0].ToolCalls)
	}
}

// Every tool is offered on every turn. A loop that sends the tool list once and
// then drops it produces a model that suddenly cannot look anything up.
func TestToolsAreOfferedEveryTurn(t *testing.T) {
	script := &ScriptedCompleter{Responses: []Response{
		Calls(Usage{}, badCall("toolu_a")),
		Says("Done.", Usage{}),
	}}
	if _, err := testLoop(t, script, Budget{MaxTurns: 8, MaxCostMicros: 1_000_000}).
		Run(context.Background(), "system", "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, req := range script.Requests {
		if len(req.Tools) != 4 {
			t.Errorf("request %d offered %d tools, want 4", i, len(req.Tools))
		}
		if req.System != "system" {
			t.Errorf("request %d lost the system prompt", i)
		}
	}
}

// A model that keeps calling tools has to be stopped by something.
func TestTheTurnCeilingStops(t *testing.T) {
	responses := make([]Response, 20)
	for i := range responses {
		responses[i] = Calls(Usage{InputTokens: 100}, badCall("toolu_x"))
	}
	script := &ScriptedCompleter{Responses: responses}

	result, err := testLoop(t, script, Budget{MaxTurns: 5, MaxCostMicros: 100_000_000}).
		Run(context.Background(), "system", "loop forever")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Halt != HaltTurnCeiling {
		t.Errorf("halt = %q, want %q", result.Halt, HaltTurnCeiling)
	}
	if result.Halt.Done() {
		t.Error("a run stopped by the turn ceiling reported itself as done")
	}
	if script.Calls() != 5 {
		t.Errorf("%d calls, want exactly the 5 the ceiling allows", script.Calls())
	}
}

// And a model that calls few tools but expensive ones has to be stopped by the
// other ceiling, or an automation can outspend the chargeback it is working on.
func TestTheBudgetStopsBeforeTheTurnCeiling(t *testing.T) {
	responses := make([]Response, 20)
	for i := range responses {
		responses[i] = Calls(Usage{InputTokens: 100_000, OutputTokens: 1000}, badCall("toolu_x"))
	}
	script := &ScriptedCompleter{Responses: responses}

	// 100k in + 1k out = 300_000 + 15_000 = 315_000 micros per turn.
	result, err := testLoop(t, script, Budget{MaxTurns: 20, MaxCostMicros: 700_000}).
		Run(context.Background(), "system", "spend")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Halt != HaltBudget {
		t.Fatalf("halt = %q, want %q", result.Halt, HaltBudget)
	}
	// Two turns are affordable; the third is refused before it is paid for.
	if script.Calls() != 3 {
		t.Errorf("%d calls, want 3 - two under the ceiling and one that crossed it", script.Calls())
	}
	if result.CostMicros <= 700_000 {
		t.Errorf("cost = %d, expected the recorded cost to include the turn that crossed", result.CostMicros)
	}
}

// The property the rest of the harness leans on: a cut-off draft must never
// look like a finished one.
func TestATruncatedAnswerIsNotDone(t *testing.T) {
	script := &ScriptedCompleter{Responses: []Response{
		Truncated("The cardholder claims the item never arri", Usage{OutputTokens: 2048}),
	}}

	result, err := testLoop(t, script, Budget{MaxTurns: 8, MaxCostMicros: 1_000_000}).
		Run(context.Background(), "system", "draft it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Halt != HaltTruncated {
		t.Errorf("halt = %q, want %q", result.Halt, HaltTruncated)
	}
	if result.Halt.Done() {
		t.Error("a response cut off at max_tokens reported itself as done")
	}
	if result.Text == "" {
		t.Error("the partial text was dropped; it is evidence for the trace")
	}
}

// Order does not change correctness - each result carries its own id - but a
// trace whose order moves between runs cannot be diffed, and the eval set
// depends on diffing it.
func TestParallelToolResultsKeepTheirOrder(t *testing.T) {
	script := &ScriptedCompleter{Responses: []Response{
		Calls(Usage{}, badCall("toolu_1"), badCall("toolu_2"), badCall("toolu_3")),
		Says("Done.", Usage{}),
	}}
	if _, err := testLoop(t, script, Budget{MaxTurns: 8, MaxCostMicros: 1_000_000}).
		Run(context.Background(), "system", "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	blocks := script.Requests[1].Messages[2].Content
	want := []string{"toolu_1", "toolu_2", "toolu_3"}
	if len(blocks) != len(want) {
		t.Fatalf("%d results for 3 calls", len(blocks))
	}
	for i := range want {
		if blocks[i].ToolUseID != want[i] {
			t.Errorf("result %d is for %q, want %q", i, blocks[i].ToolUseID, want[i])
		}
	}
}

// stop_reason said tools, the content carried none. Continuing would send an
// empty result array and get the same answer back for as many turns as the
// ceiling allows.
func TestToolUseWithNoToolBlockDoesNotSpin(t *testing.T) {
	script := &ScriptedCompleter{Responses: []Response{
		{Content: []ContentBlock{{Type: "text", Text: "hm"}}, StopReason: "tool_use"},
	}}
	result, err := testLoop(t, script, Budget{MaxTurns: 8, MaxCostMicros: 1_000_000}).
		Run(context.Background(), "system", "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Halt != HaltStopReason {
		t.Errorf("halt = %q, want %q", result.Halt, HaltStopReason)
	}
	if script.Calls() != 1 {
		t.Errorf("%d calls; the loop kept going on a turn that could not progress", script.Calls())
	}
}

// A stop_reason added to the API after this was written is recorded, not
// guessed at, and it does not count as finishing.
func TestAnUnknownStopReasonHalts(t *testing.T) {
	script := &ScriptedCompleter{Responses: []Response{
		{Content: []ContentBlock{{Type: "text", Text: "no"}}, StopReason: "refusal"},
	}}
	result, err := testLoop(t, script, Budget{MaxTurns: 8, MaxCostMicros: 1_000_000}).
		Run(context.Background(), "system", "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Halt != HaltStopReason || result.StopReason != "refusal" {
		t.Errorf("halt = %q, stop_reason = %q", result.Halt, result.StopReason)
	}
}

// A cancelled context ends the run rather than being absorbed as a tool error.
func TestACancelledContextStopsTheRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	script := &ScriptedCompleter{Responses: []Response{Says("hi", Usage{})}}
	_, err := testLoop(t, script, Budget{MaxTurns: 8, MaxCostMicros: 1_000_000}).
		Run(ctx, "system", "go")
	if err == nil {
		t.Fatal("a cancelled context did not stop the run")
	}
	if script.Calls() != 0 {
		t.Errorf("the model was called %d times after cancellation", script.Calls())
	}
}

// An exhausted script means the loop went further than the test described, and
// that has to surface as a failure rather than a repeated last answer.
func TestAnExhaustedScriptIsAnError(t *testing.T) {
	script := &ScriptedCompleter{Responses: []Response{Calls(Usage{}, badCall("toolu_a"))}}
	_, err := testLoop(t, script, Budget{MaxTurns: 8, MaxCostMicros: 1_000_000}).
		Run(context.Background(), "system", "go")
	if err == nil || !strings.Contains(err.Error(), "no response for call 2") {
		t.Fatalf("err = %v, want an exhausted-script error", err)
	}
}

// Cache tokens bill at their own rates, and an unset rate has to fall back in
// the direction that overstates rather than understates the bill.
func TestCacheRatesFallBackToInput(t *testing.T) {
	p := Pricing{InputMicrosPerMTok: 3_000_000, OutputMicrosPerMTok: 15_000_000}
	if got := p.cost(Usage{CacheReadInputTokens: 1_000_000}); got != 3_000_000 {
		t.Errorf("unset cache read rate cost %d, want the input rate 3000000", got)
	}

	p.CacheReadMicrosPerMTok = 300_000
	if got := p.cost(Usage{CacheReadInputTokens: 1_000_000}); got != 300_000 {
		t.Errorf("configured cache read rate cost %d, want 300000", got)
	}
}
