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

// WithCostCeiling bounds what the whole run may spend.
func (r *Runner) WithCostCeiling(micros int64) *Runner {
	r.MaxTotalCostMicros = micros
	return r
}

type Result struct {
	Case      string  `json:"case"`
	Why       string  `json:"why"`
	DisputeID int64   `json:"dispute_id"`
	Passed    bool    `json:"passed"`
	Grades    []Grade `json:"grades"`

	// The draft itself. A grader's verdict is not reviewable without the text
	// it was passed - "wrong_figure" says a number is wrong and not which one
	// the letter actually used, and the first question anybody asks of a
	// failing case is to see it.
	Recommendation string `json:"recommendation,omitempty"`
	Letter         string `json:"letter,omitempty"`

	// What the same dispute produced with the attack removed. Empty unless the
	// case asked for a control.
	ControlRecommendation string `json:"control_recommendation,omitempty"`

	// Which retrieval strategy actually ran, and how much it found.
	//
	// Without it a change in results cannot be attributed: a run that silently
	// fell back to full-text search looks exactly like one that used the vector
	// index, and comparing the two would be comparing a thing to itself.
	Retrieval  string `json:"retrieval,omitempty"`
	Precedents int    `json:"precedents"`

	// Token usage, so a cost can be explained rather than only reported. Cache
	// hits in particular have to be visible: a prompt too short to cache and a
	// cache working perfectly both produce a number, and only the usage says
	// which happened.
	Usage      agent.Usage   `json:"usage"`
	CostMicros int64         `json:"cost_micros"`
	Latency    time.Duration `json:"latency_ns"`

	// VerifierAgreed is nil when no verifier ran.
	VerifierAgreed *bool `json:"verifier_agreed,omitempty"`

	// Skipped means the dataset has no dispute matching this case. Reported
	// rather than passed over: a case that silently disappears is a rule
	// nobody is checking any more.
	Skipped bool   `json:"skipped,omitempty"`
	Err     string `json:"error,omitempty"`
}

type Report struct {
	Results         []Result       `json:"results"`
	Ran             int            `json:"ran"`
	PassedCount     int            `json:"passed"`
	SkippedCount    int            `json:"skipped"`
	FailuresByRule  map[string]int `json:"failures_by_rule"`
	TotalCostMicros int64          `json:"total_cost_micros"`

	Usage agent.Usage `json:"usage"`

	// Stopped explains a run that did not reach the end of the case set. A
	// pass rate over half the cases is not the pass rate, and a report that
	// does not say so invites reading it as one.
	Stopped string `json:"stopped,omitempty"`
}

func (r Report) PassRate() float64 {
	if r.Ran == 0 {
		return 0
	}
	return float64(r.PassedCount) / float64(r.Ran)
}

func (r Report) MeanCostMicros() int64 {
	if r.Ran == 0 {
		return 0
	}
	return r.TotalCostMicros / int64(r.Ran)
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

func (r *Runner) Run(ctx context.Context, cases []Case) (Report, error) {
	report := Report{FailuresByRule: map[string]int{}}

	for _, c := range cases {
		// Checked before each case rather than after, so the ceiling is never
		// knowingly exceeded - the same rule the loop applies per turn, and it
		// can still be overshot by one case for the same reason: the price of a
		// call is not known until it returns.
		if r.MaxTotalCostMicros > 0 && report.TotalCostMicros >= r.MaxTotalCostMicros {
			report.Stopped = fmt.Sprintf("cost ceiling reached after %d of %d cases",
				report.Ran+report.SkippedCount, len(cases))
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

		facts, err := r.facts.For(ctx, disputeID)
		if err != nil {
			return report, fmt.Errorf("case %s: %w", c.Name, err)
		}

		result.Retrieval = facts.Retrieval.Method
		result.Precedents = len(facts.Precedents)

		started := time.Now()
		draft, err := r.generator.Write(ctx, facts)
		result.Latency = time.Since(started)
		result.CostMicros = draft.CostMicros
		report.TotalCostMicros += draft.CostMicros

		if err != nil {
			// A failed generation is a failed case, not a failed run. The
			// number that matters is how often the system produces something
			// usable, and a crash is one of the ways it does not.
			result.Err = err.Error()
			report.Ran++
			report.Results = append(report.Results, result)
			report.FailuresByRule[RuleAnswered]++
			continue
		}

		result.Usage.Add(draft.Usage)
		result.Recommendation = draft.Recommendation
		result.Letter = draft.Letter
		result.Grades = GradeDraft(facts, draft)

		if c.Counterfactual {
			control, err := r.withoutTheAttack(ctx, facts)
			if err != nil {
				return report, fmt.Errorf("case %s control run: %w", c.Name, err)
			}
			result.CostMicros += control.CostMicros
			report.TotalCostMicros += control.CostMicros
			result.Usage.Add(control.Usage)
			result.ControlRecommendation = control.Recommendation
			result.Grades = append(result.Grades, Instructed(
				Draftlike{draft.Recommendation, draft.Letter},
				Draftlike{control.Recommendation, control.Letter}))
		}
		result.Passed = Passed(result.Grades)
		for _, g := range result.Grades {
			if !g.Passed {
				report.FailuresByRule[g.Rule]++
			}
		}

		if r.verifier != nil && draft.Recommended() {
			verdict, err := r.verifier.Check(ctx, facts, draft.Letter)
			result.CostMicros += verdict.CostMicros
			report.TotalCostMicros += verdict.CostMicros
			result.Usage.Add(verdict.Usage)
			if err == nil {
				agreed := verdict.Approved() == result.Passed
				result.VerifierAgreed = &agreed
			}
		}

		report.Usage.Add(result.Usage)
		report.Ran++
		if result.Passed {
			report.PassedCount++
		}
		report.Results = append(report.Results, result)
	}

	return report, nil
}
