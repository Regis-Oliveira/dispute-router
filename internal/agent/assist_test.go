package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/internal/api"

	"github.com/regisoliveira/dispute-router/internal/llm"
	"github.com/regisoliveira/dispute-router/internal/llm/llmtest"
)

// The whole flow against a scripted model: hold, read the record, draft,
// check, record, move. Nothing here reaches a network.
func testAssistant(t *testing.T, script *llmtest.ScriptedCompleter, maxCostMicros int64) (*Assistant, *pgxpool.Pool) {
	t.Helper()
	pool := scratchDB(t)
	store := api.NewStore(pool)
	pricing := llm.Pricing{InputMicrosPerMTok: 3_000_000, OutputMicrosPerMTok: 15_000_000}
	assistant := NewAssistant(
		factSource(t, store, fakeEvidence{}, FactSourceOptions{}),
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
	script := &llmtest.ScriptedCompleter{}         // no responses: any call is a failure
	assistant, pool := testAssistant(t, script, 1) // one micro-dollar
	id := fixture(t, pool, "chargeback", 72*time.Hour)

	outcome, err := assistant.Assist(t.Context(), id)
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

// A budget stop on the last allowed attempt goes in front of a person. Before,
// it returned the dispute to the queue, and once the attempts were used up the
// dispute silently stopped being a candidate: nobody drafted, nobody was told.
func TestABudgetStopOnTheLastAttemptReachesAPerson(t *testing.T) {
	script := &llmtest.ScriptedCompleter{}
	assistant, pool := testAssistant(t, script, 1)
	assistant.maxAttempts = 1
	id := fixture(t, pool, "chargeback", 72*time.Hour)

	outcome, err := assistant.Assist(t.Context(), id)
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	if outcome != OutcomeBudget {
		t.Fatalf("outcome = %q, want %q", outcome, OutcomeBudget)
	}
	if got := stateOf(t, pool, id); got != "draft_ready" {
		t.Errorf("state = %q, want draft_ready: a budget stop on the last attempt must reach a reviewer", got)
	}
}

// The ceiling is per dispute. A second attempt starts with the first one's
// bill already counted, so two attempts cannot spend two ceilings.
func TestTheCeilingCountsEarlierAttempts(t *testing.T) {
	script := &llmtest.ScriptedCompleter{Responses: []llm.Response{
		draftResponse(t, draftInput{Recommendation: RecommendRepresent, Letter: "Reason code 10.4; the charge of 41.00 USD stands."}, llm.Usage{InputTokens: 900, OutputTokens: 100}),
		verdictResponse(t, verdictInput{Pass: false, Findings: []Finding{{Check: CheckUnsupportedClaim, Quote: "stands", Why: "unsupported"}}}, llm.Usage{InputTokens: 1000, OutputTokens: 40}),
	}}
	assistant, pool := testAssistant(t, script, 1_000_000)
	id := fixture(t, pool, "chargeback", 72*time.Hour)

	if outcome, err := assistant.Assist(t.Context(), id); err != nil || outcome != OutcomeRejected {
		t.Fatalf("first attempt: outcome %q, err %v", outcome, err)
	}

	// A ceiling the first attempt fitted under and the second cannot: what the
	// first attempt cost, plus one. If earlier spend were not counted the
	// second attempt would see a fresh ceiling and buy tokens.
	var spent int64
	if err := pool.QueryRow(t.Context(),
		"SELECT cost_micros FROM agent_runs WHERE dispute_id = $1", id).Scan(&spent); err != nil {
		t.Fatalf("read spend: %v", err)
	}
	assistant.maxCostMicros = spent + 1
	outcome, err := assistant.Assist(t.Context(), id)
	if err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if outcome != OutcomeBudget {
		t.Errorf("second attempt outcome = %q, want %q: the first attempt's spend was not counted", outcome, OutcomeBudget)
	}
	if script.Calls() != 2 {
		t.Errorf("the second attempt bought tokens with the ceiling already spent (%d calls)", script.Calls())
	}
}

// The retry knows why the last draft was refused. It used to be an identical
// re-roll: same record, same prompt, a model that had never heard the objection.
func TestTheSecondAttemptIsToldWhyTheFirstWasRejected(t *testing.T) {
	script := &llmtest.ScriptedCompleter{Responses: []llm.Response{
		draftResponse(t, draftInput{Recommendation: RecommendRepresent, Letter: "The parcel was signed for. Reason code 10.4."}, llm.Usage{InputTokens: 900, OutputTokens: 100}),
		verdictResponse(t, verdictInput{Pass: false, Findings: []Finding{{Check: CheckUnsupportedClaim, Quote: "signed for", Why: "no signature is on file"}}}, llm.Usage{InputTokens: 1000, OutputTokens: 40}),
		draftResponse(t, draftInput{Recommendation: RecommendRepresent, Letter: "The charge of 41.00 USD was authorised. Reason code 10.4."}, llm.Usage{InputTokens: 950, OutputTokens: 100}),
		verdictResponse(t, verdictInput{Pass: true}, llm.Usage{InputTokens: 1000, OutputTokens: 20}),
	}}
	assistant, pool := testAssistant(t, script, 250_000)
	id := fixture(t, pool, "chargeback", 72*time.Hour)

	if outcome, err := assistant.Assist(t.Context(), id); err != nil || outcome != OutcomeRejected {
		t.Fatalf("first attempt: outcome %q, err %v", outcome, err)
	}
	if outcome, err := assistant.Assist(t.Context(), id); err != nil || outcome != OutcomeDrafted {
		t.Fatalf("second attempt: outcome %q, err %v", outcome, err)
	}

	first := script.Requests[0].Messages[0].Content[0].Text
	second := script.Requests[2].Messages[0].Content[0].Text
	if strings.Contains(first, "PRIOR REVIEW") {
		t.Error("the first attempt was shown a review that did not exist yet")
	}
	if !strings.Contains(second, "PRIOR REVIEW") || !strings.Contains(second, "signed for") || !strings.Contains(second, "no signature is on file") {
		t.Errorf("the second attempt was not told what the first was refused for:\n%s", second)
	}
	// And the verifier never sees it: a finding is about a draft, not the record.
	if strings.Contains(script.Requests[3].Messages[0].Content[0].Text, "PRIOR REVIEW") {
		t.Error("the prior review reached the verifier's record")
	}
}

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

// And a zero ceiling means no ceiling, as it does for the eval runner. Read as
// "nothing may be spent" it would stop every run before its first call and
// look exactly like a working budget.
func TestAZeroCeilingIsNoCeiling(t *testing.T) {
	script := &llmtest.ScriptedCompleter{Responses: []llm.Response{
		draftResponse(t, draftInput{Recommendation: RecommendRepresent, Letter: "Reason code 10.4 does not apply; the charge of 41.00 USD was authorised."}, llm.Usage{InputTokens: 900, OutputTokens: 120}),
		verdictResponse(t, verdictInput{Pass: true}, llm.Usage{InputTokens: 1000, OutputTokens: 30}),
	}}
	assistant, pool := testAssistant(t, script, 0)
	id := fixture(t, pool, "chargeback", 72*time.Hour)

	outcome, err := assistant.Assist(t.Context(), id)
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
	script := &llmtest.ScriptedCompleter{Responses: []llm.Response{
		draftResponse(t, draftInput{Recommendation: RecommendInsufficient, Letter: "Nothing on file supports a rebuttal. The merchant accepts liability."}, llm.Usage{InputTokens: 900, OutputTokens: 60}),
		verdictResponse(t, verdictInput{Pass: false, Findings: []Finding{{
			Check: CheckPromise, Quote: "The merchant accepts liability.", Why: "commits the merchant",
		}}}, llm.Usage{InputTokens: 1000, OutputTokens: 60}),
	}}
	assistant, pool := testAssistant(t, script, 250_000)
	id := fixture(t, pool, "chargeback", 72*time.Hour)

	outcome, err := assistant.Assist(t.Context(), id)
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

func hexOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}
