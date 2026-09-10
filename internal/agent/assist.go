package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Assistant is the whole flow for one dispute: hold it, read the record, draft,
// check, record, hand it to a person.
//
// It never submits anything. draft_ready is as far as it goes, and there is no
// code path from here to 'represented' - that transition belongs to whoever
// clicks approve. This is not caution for a demo. The system already declines
// to concede a chargeback automatically (internal/worker/rules.go); writing to
// a card network on a merchant's behalf is the same kind of decision.
type Assistant struct {
	facts     *FactSource
	generator *Generator
	verifier  *Verifier
	runs      *Runs
	log       *slog.Logger

	model       string
	toolSurface []string

	// MaxCostMicros bounds one dispute across both calls. An automation that
	// costs more than the chargeback it is working on has inverted the business
	// case it exists to serve.
	maxCostMicros int64

	// MaxAttempts is when a dispute stops being redrafted and starts being
	// somebody's problem.
	maxAttempts int
}

type AssistantOptions struct {
	Model         string
	MaxCostMicros int64
	MaxAttempts   int
	Logger        *slog.Logger
}

func NewAssistant(facts *FactSource, generator *Generator, verifier *Verifier, runs *Runs, opts AssistantOptions) *Assistant {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 2
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Assistant{
		facts: facts, generator: generator, verifier: verifier, runs: runs,
		log:           opts.Logger,
		model:         opts.Model,
		maxCostMicros: opts.MaxCostMicros,
		maxAttempts:   opts.MaxAttempts,
		// Recorded per run rather than assumed from the code, because the code
		// changes and the row has to stay true about the run it describes.
		toolSurface: []string{RepresentmentTool, VerdictTool},
	}
}

// promptFingerprint identifies the prompts a run was made under.
//
// A hash rather than the text: when a pass rate moves, the first question is
// whether the prompts moved, and this answers it without putting the same few
// kilobytes on every row.
func promptFingerprint() string {
	sum := sha256.Sum256([]byte(generatorSystem + "\x00" + verifierSystem))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// Trace is what the run did, as stored.
type Trace struct {
	Generator   phase  `json:"generator"`
	Verifier    *phase `json:"verifier,omitempty"`
	CitationsOK bool   `json:"citations_ok"`
	Escalated   bool   `json:"escalated,omitempty"`
	Note        string `json:"note,omitempty"`
}

type phase struct {
	Usage      Usage         `json:"usage"`
	CostMicros int64         `json:"cost_micros"`
	Latency    time.Duration `json:"latency_ns"`
}

// Assist drafts for one dispute and records the result.
//
// The order of the checks is a cost decision as much as a correctness one. The
// citation check runs in host code before the verifier is paid for, because a
// draft citing a file that is not on the dispute is already rejected and asking
// a model to read it to discover that is slower, dearer and less certain.
func (a *Assistant) Assist(ctx context.Context, disputeID int64) (string, error) {
	claim, err := a.runs.Hold(ctx, disputeID)
	if err != nil {
		return "", err
	}

	outcome, run, err := a.attempt(ctx, claim)
	if err != nil {
		// Nothing was produced and nothing is recorded: this is a fault, not a
		// verdict, and the dispute goes back untouched so it can be tried
		// again once whatever broke is fixed.
		if releaseErr := a.runs.Release(ctx, claim); releaseErr != nil && !errors.Is(releaseErr, ErrClaimLost) {
			a.log.Error("could not release dispute after a failed run",
				"dispute", disputeID, "error", releaseErr)
		}
		return "", err
	}

	if err := a.runs.Record(ctx, claim, run); err != nil {
		return "", fmt.Errorf("recording run for dispute %d: %w", disputeID, err)
	}

	a.log.Info("assisted",
		"dispute", disputeID, "attempt", claim.Attempt, "outcome", outcome,
		"cost_micros", run.CostMicros, "findings", len(run.Findings))
	return outcome, nil
}

func (a *Assistant) attempt(ctx context.Context, claim Claim) (string, Run, error) {
	started := time.Now()
	run := Run{
		Model:             a.model,
		PromptFingerprint: promptFingerprint(),
		ToolSurface:       a.toolSurface,
		StartedAt:         started,
	}

	facts, err := a.facts.For(ctx, claim.DisputeID)
	if err != nil {
		return "", run, fmt.Errorf("assembling facts for dispute %d: %w", claim.DisputeID, err)
	}

	genStarted := time.Now()
	draft, err := a.generator.Write(ctx, facts)
	// The cost is real whether or not a draft came back, so it is added before
	// the error is examined. A failed run that reports zero spend is how a
	// budget silently stops meaning anything.
	run.Usage.Add(draft.Usage)
	run.CostMicros += draft.CostMicros
	trace := Trace{Generator: phase{
		Usage: draft.Usage, CostMicros: draft.CostMicros, Latency: time.Since(genStarted),
	}}
	run.Trace = &trace

	if err != nil {
		return "", run, fmt.Errorf("drafting dispute %d: %w", claim.DisputeID, err)
	}

	run.Recommendation = draft.Recommendation
	run.Letter = draft.Letter
	run.CitedEvidence = draft.CitedEvidence

	if !draft.Recommended() {
		// insufficient_evidence. There is nothing to verify - the draft makes
		// no claim about the merchant's case - and paying for a verifier call
		// to confirm that would be spending on a foregone conclusion.
		trace.Note = "no representment recommended; verifier not called"
		run.Outcome = OutcomeInsufficient
		return OutcomeInsufficient, run, nil
	}

	if findings := CheckCitations(facts, draft); len(findings) > 0 {
		run.Findings = findings
		run.Outcome = OutcomeRejected
		run.Escalated = a.lastAttempt(claim)
		trace.Escalated = run.Escalated
		trace.Note = "rejected on citations; verifier not called"
		return run.Outcome, run, nil
	}
	trace.CitationsOK = true

	if run.CostMicros >= a.maxCostMicros {
		// Checked between the two calls, which is the only place it can be
		// checked: the price of a call is not known until it returns.
		run.Outcome = OutcomeBudget
		trace.Note = "budget spent on the draft; verifier not called"
		return OutcomeBudget, run, nil
	}

	verStarted := time.Now()
	verdict, err := a.verifier.Check(ctx, facts, draft.Letter)
	run.Usage.Add(verdict.Usage)
	run.CostMicros += verdict.CostMicros
	trace.Verifier = &phase{
		Usage: verdict.Usage, CostMicros: verdict.CostMicros, Latency: time.Since(verStarted),
	}
	if err != nil {
		return "", run, fmt.Errorf("verifying dispute %d: %w", claim.DisputeID, err)
	}

	run.Findings = verdict.Findings

	if !verdict.Approved() {
		run.Outcome = OutcomeRejected
		run.Escalated = a.lastAttempt(claim)
		trace.Escalated = run.Escalated
		return run.Outcome, run, nil
	}

	run.Outcome = OutcomeDrafted
	return OutcomeDrafted, run, nil
}

// lastAttempt reports whether this run has used up the retries.
//
// A rejected draft is recorded as rejected either way - the verdict does not
// improve because the clock ran out. What changes is where the dispute goes:
// below the ceiling it returns to the queue for another try, and at the ceiling
// it goes in front of a person, findings attached, because retrying has stopped
// working and the deadline has not stopped moving.
func (a *Assistant) lastAttempt(claim Claim) bool {
	return claim.Attempt >= a.maxAttempts
}
