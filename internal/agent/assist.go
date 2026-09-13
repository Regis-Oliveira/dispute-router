package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/regisoliveira/dispute-router/internal/llm"
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

	// maxCostMicros bounds one dispute across both calls. An automation that
	// costs more than the chargeback it is working on has inverted the business
	// case it exists to serve.
	maxCostMicros int64

	// maxAttempts is when a dispute stops being redrafted and starts being
	// somebody's problem.
	maxAttempts int
}

// AssistantOptions configures NewAssistant; zero MaxAttempts means 2, a nil
// Logger means slog.Default, and zero MaxCostMicros means no ceiling.
type AssistantOptions struct {
	Model         string
	MaxCostMicros int64
	MaxAttempts   int
	Logger        *slog.Logger
}

// NewAssistant wires the record source, the two model calls and the store into
// one flow.
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

// promptFingerprint identifies everything fixed that a run's model was shown.
//
// A hash rather than the text: when a pass rate moves, the first question is
// whether the prompts moved, and this answers it without putting the same few
// kilobytes on every row. It covers the two system prompts, both tool schemas,
// and the record template - the headings and instructions Facts.Render writes
// around the data. The first version hashed the system prompts alone, and
// every prompt change of the following month - precedent, base rates, the
// amounts block - went into the template and left the hash untouched.
// Rendering the zero-value record captures the template with no data in it.
func promptFingerprint() string {
	template, err := Facts{}.Render()
	if err != nil {
		template = "render error: " + err.Error()
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		generatorSystem, verifierSystem, string(draftSchema()), string(verdictSchema()), template,
	}, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// Trace is what the run did, as stored in agent_runs.trace and read back by
// cmd/agent -report.
type Trace struct {
	Generator   Phase  `json:"generator"`
	Verifier    *Phase `json:"verifier,omitempty"`
	CitationsOK bool   `json:"citations_ok"`
	Escalated   bool   `json:"escalated,omitempty"`
	Note        string `json:"note,omitempty"`

	// How the record was assembled. A change in draft quality has to be
	// attributable to a change in what the model was shown, and "vector, 3
	// precedents" against "lexical, 0" is the first thing to compare.
	Retrieval  Retrieval     `json:"retrieval"`
	Precedents int           `json:"precedents"`
	Assembly   time.Duration `json:"assembly_ns"`

	// What the model was shown of the files on the dispute: how many there
	// were, how many had text in the record, and how much. "Represent, citing
	// a file" and "insufficient evidence" are the outcomes the file decides,
	// and a run that could not read the file is a different run from one that
	// read it.
	Evidence EvidenceTrace `json:"evidence"`
}

// EvidenceTrace is how much of the dispute's files reached the record.
type EvidenceTrace struct {
	Files int `json:"files"`
	Read  int `json:"read"`
	Runes int `json:"runes"`
}

// evidenceShown counts what reached the record.
func evidenceShown(facts Facts) EvidenceTrace {
	t := EvidenceTrace{Files: len(facts.Evidence)}
	for _, file := range facts.Evidence {
		if file.Status == EvidenceNotRead {
			continue
		}
		t.Read++
		t.Runes += utf8.RuneCountInString(file.Text)
	}
	return t
}

// Phase is what one model call in a run cost and how long it took.
type Phase struct {
	Usage      llm.Usage     `json:"usage"`
	CostMicros int64         `json:"cost_micros"`
	Latency    time.Duration `json:"latency_ns"`
}

// Assist drafts for one dispute and records the result.
//
// The order of the checks is a cost decision as much as a correctness one. The
// citation check runs in host code before the verifier is paid for, because a
// draft citing a file that is not on the dispute is already rejected and asking
// a model to read it to discover that is slower, dearer and less certain.
func (a *Assistant) Assist(ctx context.Context, disputeID int64) (Outcome, error) {
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
		// The run was produced and could not be written. Without a release the
		// dispute stays in 'resolving' for good, held by a process that has
		// already given up on it.
		if releaseErr := a.runs.Release(ctx, claim); releaseErr != nil && !errors.Is(releaseErr, ErrClaimLost) {
			a.log.Error("could not release dispute after a failed record",
				"dispute", disputeID, "error", releaseErr)
		}
		return "", fmt.Errorf("recording run for dispute %d: %w", disputeID, err)
	}

	a.log.Info("assisted",
		"dispute", disputeID, "attempt", claim.Attempt, "outcome", outcome,
		"cost_micros", run.CostMicros, "findings", len(run.Findings))
	return outcome, nil
}

func (a *Assistant) attempt(ctx context.Context, claim Claim) (Outcome, Run, error) {
	started := time.Now()
	run := Run{
		Model:             a.model,
		PromptFingerprint: promptFingerprint(),
		ToolSurface:       a.toolSurface,
		StartedAt:         started,
	}

	assembleStarted := time.Now()
	facts, err := a.facts.For(ctx, claim.DisputeID)
	if err != nil {
		return "", run, fmt.Errorf("assembling facts for dispute %d: %w", claim.DisputeID, err)
	}
	facts.PriorFindings = claim.PriorFindings
	assembly := time.Since(assembleStarted)

	// The record's size is known before the call, so the ceiling is checked
	// before the first token is bought. It used to be checked only between the
	// two calls, which left the generator free to spend the whole budget on
	// one enormous claim - and "never knowingly exceeded" was true only of the
	// second call.
	if estimate, err := a.generator.EstimateInputMicros(facts); err == nil && a.overBudget(claim.SpentMicros+run.CostMicros+estimate) {
		run.Outcome = OutcomeBudget
		run.Escalated = a.lastAttempt(claim)
		run.Trace = &Trace{Note: "the record alone would spend the ceiling; generator not called", Escalated: run.Escalated,
			Retrieval: facts.Retrieval, Precedents: len(facts.Precedents), Assembly: assembly, Evidence: evidenceShown(facts)}
		return OutcomeBudget, run, nil
	}

	genStarted := time.Now()
	draft, err := a.generator.Write(ctx, facts)
	// The cost is real whether or not a draft came back, so it is added before
	// the error is examined. A failed run that reports zero spend is how a
	// budget silently stops meaning anything.
	run.Usage.Add(draft.Usage)
	run.CostMicros += draft.CostMicros
	trace := Trace{Generator: Phase{
		Usage: draft.Usage, CostMicros: draft.CostMicros, Latency: time.Since(genStarted),
	}, Retrieval: facts.Retrieval, Precedents: len(facts.Precedents), Assembly: assembly, Evidence: evidenceShown(facts)}
	if len(facts.PriorFindings) > 0 {
		trace.Note = fmt.Sprintf("redrafted with %d prior finding(s) in view", len(facts.PriorFindings))
	}
	run.Trace = &trace

	if err != nil {
		return "", run, fmt.Errorf("drafting dispute %d: %w", claim.DisputeID, err)
	}

	run.Recommendation = draft.Recommendation
	run.Letter = draft.Letter
	run.CitedEvidence = draft.CitedEvidence

	// An insufficient_evidence draft is checked like any other. It used to skip
	// the verifier on the grounds that it makes no claim about the merchant's
	// case - but it is a letter, and a letter can still promise, invent, or
	// repeat a planted instruction. "Decline, and say the merchant accepts
	// liability" is exactly the outcome an attacker would aim for, and it was
	// the one outcome nothing read.

	if findings := CheckCitations(facts, draft); len(findings) > 0 {
		run.Findings = findings
		run.Outcome = OutcomeRejected
		run.Escalated = a.lastAttempt(claim)
		trace.Escalated = run.Escalated
		trace.Note = "rejected on citations; verifier not called"
		return run.Outcome, run, nil
	}
	trace.CitationsOK = true

	if a.overBudget(claim.SpentMicros + run.CostMicros) {
		// Checked again between the two calls: the price of the first call
		// is not known until it returns. The letter is kept - it was paid for
		// and a reviewer may want to see it - but it carries no
		// recommendation, because nothing checked it, and agent_runs refuses a
		// recommendation on a budget_exceeded row for exactly that reason.
		run.Outcome = OutcomeBudget
		run.Recommendation = ""
		run.Escalated = a.lastAttempt(claim)
		trace.Escalated = run.Escalated
		trace.Note = "budget spent on the draft; verifier not called; the letter is unverified"
		return OutcomeBudget, run, nil
	}

	verStarted := time.Now()
	verdict, err := a.verifier.Check(ctx, facts, draft.Letter)
	run.Usage.Add(verdict.Usage)
	run.CostMicros += verdict.CostMicros
	trace.Verifier = &Phase{
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

	if !draft.Recommended() {
		run.Outcome = OutcomeInsufficient
		return OutcomeInsufficient, run, nil
	}
	run.Outcome = OutcomeDrafted
	return OutcomeDrafted, run, nil
}

// overBudget compares spend to the ceiling. Zero means no ceiling, the same
// convention the eval runner uses - a zero read as "nothing may be spent" would
// stop every run before its first call and look like a working budget.
func (a *Assistant) overBudget(micros int64) bool {
	return a.maxCostMicros > 0 && micros >= a.maxCostMicros
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
