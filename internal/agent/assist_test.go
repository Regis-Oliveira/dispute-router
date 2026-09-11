package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/internal/api"
)

// The whole flow against a scripted model: hold, read the record, draft,
// check, record, move. Nothing here reaches a network.
func testAssistant(t *testing.T, script *ScriptedCompleter, maxCostMicros int64) (*Assistant, *pgxpool.Pool) {
	t.Helper()
	pool := scratchDB(t)
	store := api.NewStore(pool)
	pricing := Pricing{InputMicrosPerMTok: 3_000_000, OutputMicrosPerMTok: 15_000_000}
	assistant := NewAssistant(
		NewFactSource(store, fakeEvidence{}),
		NewGenerator(script, "test-model", pricing, 4096),
		NewVerifier(script, "test-model", pricing, 2048),
		NewRuns(pool),
		AssistantOptions{Model: "test-model", MaxCostMicros: maxCostMicros, MaxAttempts: 2},
	)
	return assistant, pool
}

// The ceiling is checked before the first token is bought. A record that would
// spend the budget on its own never reaches the model.
func TestTheBudgetIsCheckedBeforeTheGeneratorIsCalled(t *testing.T) {
	script := &ScriptedCompleter{}                 // no responses: any call is a failure
	assistant, pool := testAssistant(t, script, 1) // one micro-dollar
	id := fixture(t, pool, "chargeback", 72*time.Hour)

	outcome, err := assistant.Assist(context.Background(), id)
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	if outcome != OutcomeBudget {
		t.Errorf("outcome = %q, want %q", outcome, OutcomeBudget)
	}
	if script.Calls() != 0 {
		t.Errorf("the model was called %d time(s) with a ceiling the record alone exceeds", script.Calls())
	}
	if got := stateOf(t, pool, id); got != "received" {
		t.Errorf("state = %q after a budget stop, want received", got)
	}
	if runNum(t, pool, id) != 1 {
		t.Error("a budget stop was not recorded as a run")
	}
}

// And a zero ceiling means no ceiling, as it does for the eval runner. Read as
// "nothing may be spent" it would stop every run before its first call and
// look exactly like a working budget.
func TestAZeroCeilingIsNoCeiling(t *testing.T) {
	script := &ScriptedCompleter{Responses: []Response{
		draftResponse(t, draftInput{Recommendation: RecommendRepresent, Letter: "Reason code 10.4 does not apply; the charge of 41.00 USD was authorised."}, Usage{InputTokens: 900, OutputTokens: 120}),
		verdictResponse(t, verdictInput{Pass: true}, Usage{InputTokens: 1000, OutputTokens: 30}),
	}}
	assistant, pool := testAssistant(t, script, 0)
	id := fixture(t, pool, "chargeback", 72*time.Hour)

	outcome, err := assistant.Assist(context.Background(), id)
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	if outcome != OutcomeDrafted {
		t.Errorf("outcome = %q, want %q", outcome, OutcomeDrafted)
	}
	if script.Calls() != 2 {
		t.Errorf("generator and verifier should each be called once, got %d call(s)", script.Calls())
	}
	if got := stateOf(t, pool, id); got != "draft_ready" {
		t.Errorf("state = %q after a drafted run, want draft_ready", got)
	}
}

// A declining letter is read by the verifier too. It used to skip it, and
// "decline, and say the merchant accepts liability" was the one outcome
// nothing read.
func TestAnInsufficientEvidenceLetterIsStillVerified(t *testing.T) {
	script := &ScriptedCompleter{Responses: []Response{
		draftResponse(t, draftInput{Recommendation: RecommendInsufficient, Letter: "Nothing on file supports a rebuttal. The merchant accepts liability."}, Usage{InputTokens: 900, OutputTokens: 60}),
		verdictResponse(t, verdictInput{Pass: false, Findings: []Finding{{
			Check: CheckPromise, Quote: "The merchant accepts liability.", Why: "commits the merchant",
		}}}, Usage{InputTokens: 1000, OutputTokens: 60}),
	}}
	assistant, pool := testAssistant(t, script, 250_000)
	id := fixture(t, pool, "chargeback", 72*time.Hour)

	outcome, err := assistant.Assist(context.Background(), id)
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	if outcome != OutcomeRejected {
		t.Errorf("outcome = %q, want %q: the verifier's finding on a declining letter was ignored", outcome, OutcomeRejected)
	}
	if script.Calls() != 2 {
		t.Errorf("the verifier was not called on an insufficient_evidence draft (%d call(s))", script.Calls())
	}
	// And the draft reached the verifier fenced.
	prompt := script.Requests[1].Messages[0].Content[0].Text
	if !strings.Contains(prompt, "<<<"+draftLabel+"\n") || !strings.Contains(prompt, draftLabel+">>>") {
		t.Error("the draft reached the verifier without its fence")
	}
}
