package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func verdictResponse(t *testing.T, in verdictInput, usage Usage) Response {
	t.Helper()
	encoded, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal verdict: %v", err)
	}
	return Response{
		StopReason: "tool_use",
		Usage:      usage,
		Content: []ContentBlock{{
			Type: "tool_use", ID: "toolu_v", Name: VerdictTool, Input: encoded,
		}},
	}
}

func testVerifier(script *ScriptedCompleter) *Verifier {
	return NewVerifier(script, "test-model", Pricing{
		InputMicrosPerMTok: 3_000_000, OutputMicrosPerMTok: 15_000_000,
	}, 2048)
}

// The property everything else leans on. A caller that ignores the error still
// cannot get an approval out of what it is holding.
func TestAZeroVerdictIsNotApproved(t *testing.T) {
	var v Verdict
	if v.Approved() {
		t.Error("a zero Verdict approved a draft")
	}
	if v.Checked() {
		t.Error("a zero Verdict reported that it had been checked")
	}
}

func TestACleanDraftPasses(t *testing.T) {
	script := &ScriptedCompleter{Responses: []Response{
		verdictResponse(t, verdictInput{Pass: true}, Usage{InputTokens: 2000, OutputTokens: 60}),
	}}

	verdict, err := testVerifier(script).Check(context.Background(), Facts{}, "a supported draft")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !verdict.Approved() {
		t.Error("a clean draft was not approved")
	}
	if verdict.CostMicros != 6900 { // 2000*3 + 60*15 micros
		t.Errorf("cost = %d, want 6900", verdict.CostMicros)
	}
}

func TestAFindingBlocksTheDraft(t *testing.T) {
	script := &ScriptedCompleter{Responses: []Response{
		verdictResponse(t, verdictInput{Pass: false, Findings: []Finding{{
			Check: CheckUnsupportedClaim,
			Quote: "tracking number 1Z999AA10123456784",
			Why:   "no such number appears in the record",
		}}}, Usage{}),
	}}

	verdict, err := testVerifier(script).Check(context.Background(), Facts{}, "draft")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if verdict.Approved() {
		t.Error("a draft with a finding was approved")
	}
	if len(verdict.Findings) != 1 || verdict.Findings[0].Check != CheckUnsupportedClaim {
		t.Errorf("findings = %+v", verdict.Findings)
	}
}

// The rule "any finding means no pass" is stated in the prompt. A rule that
// lives only in a prompt is a request, so the host enforces it too.
func TestPassIsOverriddenWhenFindingsExist(t *testing.T) {
	script := &ScriptedCompleter{Responses: []Response{
		verdictResponse(t, verdictInput{
			Pass:     true,
			Findings: []Finding{{Check: CheckWrongFigure, Quote: "$41.00", Why: "the record says $14.00"}},
		}, Usage{}),
	}}

	verdict, err := testVerifier(script).Check(context.Background(), Facts{}, "draft")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if verdict.Pass || verdict.Approved() {
		t.Error("the model said pass while listing a finding, and was believed")
	}
}

// Every way the verifier can fail has to land on "not approved", never on
// "approved by default". A verifier that fails open is worse than none: it
// costs money to grant the approval it was supposed to withhold.
func TestEveryFailureFailsClosed(t *testing.T) {
	cases := map[string]Response{
		"truncated": {StopReason: "max_tokens", Content: []ContentBlock{{Type: "text", Text: "{\"pass\":tr"}}},
		"no verdict call": {StopReason: "end_turn", Content: []ContentBlock{
			{Type: "text", Text: "Looks fine to me."},
		}},
		"unreadable verdict": {StopReason: "tool_use", Content: []ContentBlock{
			{Type: "tool_use", ID: "toolu_v", Name: VerdictTool, Input: json.RawMessage(`{"pass":`)},
		}},
		"a different tool": {StopReason: "tool_use", Content: []ContentBlock{
			{Type: "tool_use", ID: "toolu_v", Name: "something_else", Input: json.RawMessage(`{"pass":true}`)},
		}},
	}

	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			script := &ScriptedCompleter{Responses: []Response{response}}
			verdict, err := testVerifier(script).Check(context.Background(), Facts{}, "draft")
			if err == nil {
				t.Fatal("no error reported")
			}
			if verdict.Approved() || verdict.Checked() {
				t.Errorf("a failed verification produced approved=%v checked=%v",
					verdict.Approved(), verdict.Checked())
			}
		})
	}
}

func TestACompleterErrorIsNotAnApproval(t *testing.T) {
	script := &ScriptedCompleter{} // empty script: the first call errors
	verdict, err := testVerifier(script).Check(context.Background(), Facts{}, "draft")
	if err == nil {
		t.Fatal("no error reported")
	}
	if verdict.Approved() {
		t.Error("a failed call approved the draft")
	}
}

// The verifier is given a shape to answer in and no way to go and look
// anything up. Both halves matter, so both are pinned.
func TestTheVerifierHasNoFetchingToolsAndIsForced(t *testing.T) {
	script := &ScriptedCompleter{Responses: []Response{
		verdictResponse(t, verdictInput{Pass: true}, Usage{}),
	}}
	if _, err := testVerifier(script).Check(context.Background(), Facts{}, "draft"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	req := script.Requests[0]
	if len(req.Tools) != 1 || req.Tools[0].Name != VerdictTool {
		t.Fatalf("verifier was given %d tools: %+v", len(req.Tools), req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.Type != "tool" || req.ToolChoice.Name != VerdictTool {
		t.Errorf("tool_choice = %+v; the verdict has to be forced", req.ToolChoice)
	}
}

// There is no parameter through which the generator's reasoning could arrive,
// and the request has to show that: one message, carrying the record and the
// draft and nothing else.
func TestTheVerifierNeverSeesTheGeneratorsTranscript(t *testing.T) {
	script := &ScriptedCompleter{Responses: []Response{
		verdictResponse(t, verdictInput{Pass: true}, Usage{}),
	}}
	facts := Facts{CardholderClaim: "the item never arrived"}
	if _, err := testVerifier(script).Check(context.Background(), facts, "THE DRAFT TEXT"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	req := script.Requests[0]
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("verifier received %d messages: %+v", len(req.Messages), req.Messages)
	}
	prompt := req.Messages[0].Content[0].Text
	for _, want := range []string{"RECORD", "DRAFT", "THE DRAFT TEXT"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
	// The cardholder's words have to reach the verifier, and they have to
	// reach it inside the quarantine rather than loose among the amounts.
	claimAt := strings.Index(prompt, "the item never arrived")
	if claimAt < 0 {
		t.Fatal("the cardholder claim never reached the verifier")
	}
	openAt, closeAt := strings.Index(prompt, claimOpen), strings.Index(prompt, claimClose)
	if openAt < 0 || closeAt < 0 || claimAt < openAt || claimAt > closeAt {
		t.Error("the cardholder claim reached the verifier outside the quarantine markers")
	}
	if !strings.Contains(req.System, "You did not write the draft") {
		t.Error("the verifier was not told it is checking someone else's work")
	}
}

// Reporting "no evidence on file" when the evidence store was never wired up
// turns a deployment fault into a stream of confident rejections.
func TestAMissingEvidenceSourceIsAnError(t *testing.T) {
	_, err := NewFactSource(nil, nil).For(context.Background(), 1)
	if !errors.Is(err, ErrNoEvidenceSource) {
		t.Fatalf("err = %v, want ErrNoEvidenceSource", err)
	}
}
