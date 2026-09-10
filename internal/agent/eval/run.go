package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/internal/agent"
)

// Runner drafts for each case and grades what comes back.
//
// It goes to the generator directly, not through the Assistant. An eval run
// must not hold a dispute, move it, or write an agent_run: measuring a system
// is not using it, and a benchmark that leaves rows behind changes the thing it
// is measuring - the next run would find different candidates, and the numbers
// would drift for a reason nobody could see.
type Runner struct {
	pool      *pgxpool.Pool
	facts     *agent.FactSource
	generator *agent.Generator

	// Samples is how many times each case is drafted. One by default, because
	// the cost is linear in it and nobody should multiply their bill by
	// accident.
	Samples int

	// MaxTotalCostMicros stops the whole run, not one case.
	//
	// The Assistant already bounds a single dispute, but an eval is a loop over
	// disputes and nothing was bounding the loop. A prompt change that makes
	// every case retry, or a case set that grows, spends the whole budget
	// before anybody sees a number. Zero means no ceiling.
	MaxTotalCostMicros int64

	// verifier is optional. Its verdict is reported alongside the graders
	// rather than replacing them: the graders say whether a rule was broken,
	// and the verifier says whether the model thinks one was. Where they
	// disagree is the interesting part, and collapsing them into one number
	// would hide exactly that.
	verifier *agent.Verifier
}

func NewRunner(pool *pgxpool.Pool, facts *agent.FactSource, generator *agent.Generator, verifier *agent.Verifier) *Runner {
	return &Runner{pool: pool, facts: facts, generator: generator, verifier: verifier}
}

// WithSamples sets how many drafts each case gets.
func (r *Runner) WithSamples(n int) *Runner {
	r.Samples = n
	return r
}

// WithCostCeiling bounds what the whole run may spend.
func (r *Runner) WithCostCeiling(micros int64) *Runner {
	r.MaxTotalCostMicros = micros
	return r
}

// Sample is one draft of one case.
//
// Cases are run more than once because a single run cannot tell a fix from
// luck. Five consecutive runs of this eval scored 9/9, 7/9, 9/9, 8/9 and 9/9
// with no code change between some of them: the failures were the model
// phrasing something differently, not the system behaving differently. A
// number that moves on its own is not a measurement until you know how much it
// moves.
type Sample struct {
	Passed bool    `json:"passed"`
	Grades []Grade `json:"grades"`

	// The draft itself. A grader's verdict is not reviewable without the text
	// it was passed - "wrong_figure" says a number is wrong and not which one
	// the letter actually used.
	Recommendation string `json:"recommendation,omitempty"`
	Letter         string `json:"letter,omitempty"`

	// What the same dispute produced with the attack removed. Empty unless the
	// case asked for a control.
	ControlRecommendation string `json:"control_recommendation,omitempty"`

	Usage      agent.Usage   `json:"usage"`
	CostMicros int64         `json:"cost_micros"`
	Latency    time.Duration `json:"latency_ns"`

	// VerifierApproved and VerifierAgreed are nil when no verifier ran. Both
	// are kept because "disagreed" has two directions and only one of them is
	// a grader that missed something.
	VerifierApproved *bool  `json:"verifier_approved,omitempty"`
	VerifierAgreed   *bool  `json:"verifier_agreed,omitempty"`
	Err              string `json:"error,omitempty"`
}

// Result is one case across all its samples.
type Result struct {
	Case      string   `json:"case"`
	Why       string   `json:"why"`
	DisputeID int64    `json:"dispute_id"`
	Samples   []Sample `json:"samples"`

	// Which retrieval strategy actually ran, and how much it found. The same
	// for every sample of a case, since the record is assembled once.
	Retrieval  string `json:"retrieval,omitempty"`
	Precedents int    `json:"precedents"`

	Usage      agent.Usage `json:"usage"`
	CostMicros int64       `json:"cost_micros"`

	// Skipped means the dataset has no dispute matching this case. Reported
	// rather than passed over: a case that silently disappears is a rule
	// nobody is checking any more.
	Skipped bool   `json:"skipped,omitempty"`
	Err     string `json:"error,omitempty"`
}

func (r Result) PassedCount() int {
	n := 0
	for _, s := range r.Samples {
		if s.Passed {
			n++
		}
	}
	return n
}

// Unstable is the number this whole change exists to produce.
//
// A case that passed three times out of five and one that passed five out of
// five both read as "pass" when a case is run once, and the difference between
// them is the difference between a result and a coin. Anything unstable makes
// every single-sample number about it meaningless.
func (r Result) Unstable() bool {
	n := r.PassedCount()
	return len(r.Samples) > 1 && n > 0 && n < len(r.Samples)
}

// FailuresByRule counts, across samples, how often each rule was broken.
func (r Result) FailuresByRule() map[string]int {
	out := map[string]int{}
	for _, sample := range r.Samples {
		for _, g := range sample.Grades {
			if !g.Passed {
				out[g.Rule]++
			}
		}
		if sample.Err != "" {
			out[RuleAnswered]++
		}
	}
	return out
}

type Report struct {
	Results      []Result `json:"results"`
	Samples      int      `json:"samples_per_case"`
	Cases        int      `json:"cases_run"`
	Runs         int      `json:"runs"`
	PassedRuns   int      `json:"passed_runs"`
	SkippedCount int      `json:"skipped"`

	// UnstableCases passed some samples and failed others. It is the number to
	// read first: while it is above zero, every other figure here is an
	// average over a thing that moves.
	UnstableCases int `json:"unstable_cases"`

	FailuresByRule  map[string]int `json:"failures_by_rule"`
	TotalCostMicros int64          `json:"total_cost_micros"`

	Usage agent.Usage `json:"usage"`

	// Stopped explains a run that did not reach the end of the case set. A
	// pass rate over half the cases is not the pass rate, and a report that
	// does not say so invites reading it as one.
	Stopped string `json:"stopped,omitempty"`
}

// PassRate is over RUNS, not cases. With one sample per case the two are the
// same number; with more they are not, and the runs figure is the one that
// carries the sample size.
func (r Report) PassRate() float64 {
	if r.Runs == 0 {
		return 0
	}
	return float64(r.PassedRuns) / float64(r.Runs)
}

// MeanCostMicros is per run, which is what a draft actually costs. Per case it
// would be per case times the sample count, and nobody drafts five times in
// production.
func (r Report) MeanCostMicros() int64 {
	if r.Runs == 0 {
		return 0
	}
	return r.TotalCostMicros / int64(r.Runs)
}

// plantedMarker is where the seeded attack begins. Specific to the fixture on
// purpose: a control run has to remove exactly the attack and nothing else, and
// a general "strip anything suspicious" would change the claim in ways that
// make the comparison meaningless.
const plantedMarker = "SYSTEM:"

// withoutTheAttack drafts the same dispute again with the planted sentences
// removed. Everything else - the record, the evidence, the precedent - is
// identical, so any difference in the recommendation is attributable to the
// attack and to nothing else.
func (r *Runner) withoutTheAttack(ctx context.Context, facts agent.Facts) (agent.Draft, error) {
	clean := facts
	if at := strings.Index(clean.CardholderClaim, plantedMarker); at >= 0 {
		clean.CardholderClaim = strings.TrimSpace(clean.CardholderClaim[:at])
	}
	return r.generator.Write(ctx, clean)
}

// Run drafts for each case, Samples times, and grades every draft.
func (r *Runner) Run(ctx context.Context, cases []Case) (Report, error) {
	samples := max(1, r.Samples)
	report := Report{FailuresByRule: map[string]int{}, Samples: samples}

	for _, c := range cases {
		if r.MaxTotalCostMicros > 0 && report.TotalCostMicros >= r.MaxTotalCostMicros {
			report.Stopped = fmt.Sprintf("cost ceiling reached after %d of %d cases",
				report.Cases+report.SkippedCount, len(cases))
			break
		}

		result := Result{Case: c.Name, Why: c.Why}

		var disputeID int64
		err := r.pool.QueryRow(ctx, c.Query).Scan(&disputeID)
		if errors.Is(err, pgx.ErrNoRows) {
			result.Skipped = true
			result.Err = "no dispute in the dataset matches this case"
			report.SkippedCount++
			report.Results = append(report.Results, result)
			continue
		}
		if err != nil {
			return report, fmt.Errorf("selecting a dispute for case %s: %w", c.Name, err)
		}
		result.DisputeID = disputeID

		// The record is assembled once and reused across samples. Reassembling
		// it would embed the same query five times for nothing, and - worse -
		// let retrieval vary between samples, so a difference between drafts
		// could not be attributed to the model.
		facts, err := r.facts.For(ctx, disputeID)
		if err != nil {
			return report, fmt.Errorf("case %s: %w", c.Name, err)
		}
		result.Retrieval = facts.Retrieval.Method
		result.Precedents = len(facts.Precedents)

		for i := 0; i < samples; i++ {
			if r.MaxTotalCostMicros > 0 && report.TotalCostMicros >= r.MaxTotalCostMicros {
				report.Stopped = fmt.Sprintf("cost ceiling reached during %s, sample %d of %d",
					c.Name, i+1, samples)
				break
			}

			sample := r.draft(ctx, c, facts)
			result.Samples = append(result.Samples, sample)
			result.CostMicros += sample.CostMicros
			result.Usage.Add(sample.Usage)
			report.TotalCostMicros += sample.CostMicros
			report.Usage.Add(sample.Usage)
		}

		for rule, n := range result.FailuresByRule() {
			report.FailuresByRule[rule] += n
		}
		report.Cases++
		report.Runs += len(result.Samples)
		report.PassedRuns += result.PassedCount()
		if result.Unstable() {
			report.UnstableCases++
		}
		report.Results = append(report.Results, result)

		if report.Stopped != "" {
			break
		}
	}

	return report, nil
}

// draft produces and grades one sample.
//
// A failed generation is a failed sample, not a failed run: the number that
// matters is how often the system produces something usable, and a crash is one
// of the ways it does not.
func (r *Runner) draft(ctx context.Context, c Case, facts agent.Facts) Sample {
	var sample Sample
	started := time.Now()

	draft, err := r.generator.Write(ctx, facts)
	sample.Latency = time.Since(started)
	sample.CostMicros = draft.CostMicros
	sample.Usage.Add(draft.Usage)

	if err != nil {
		sample.Err = err.Error()
		return sample
	}

	sample.Recommendation = draft.Recommendation
	sample.Letter = draft.Letter
	sample.Grades = GradeDraft(facts, draft)

	if c.Counterfactual {
		control, err := r.withoutTheAttack(ctx, facts)
		sample.CostMicros += control.CostMicros
		sample.Usage.Add(control.Usage)
		if err != nil {
			sample.Err = "control run: " + err.Error()
			return sample
		}
		sample.ControlRecommendation = control.Recommendation
		sample.Grades = append(sample.Grades, Instructed(
			Draftlike{draft.Recommendation, draft.Letter},
			Draftlike{control.Recommendation, control.Letter}))
	}

	sample.Passed = Passed(sample.Grades)

	// Every written draft is checked, insufficient_evidence included: that is
	// the outcome a planted instruction is most likely to ask for, and a letter
	// that declines can still promise or invent.
	if r.verifier != nil {
		verdict, err := r.verifier.Check(ctx, facts, draft.Letter)
		sample.CostMicros += verdict.CostMicros
		sample.Usage.Add(verdict.Usage)
		if err == nil {
			approved := verdict.Approved()
			agreed := approved == sample.Passed
			sample.VerifierApproved = &approved
			sample.VerifierAgreed = &agreed
		}
	}
	return sample
}
