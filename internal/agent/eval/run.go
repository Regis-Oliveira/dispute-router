package eval

import (
	"context"
	"errors"
	"fmt"
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

type Result struct {
	Case       string        `json:"case"`
	Why        string        `json:"why"`
	DisputeID  int64         `json:"dispute_id"`
	Passed     bool          `json:"passed"`
	Grades     []Grade       `json:"grades"`
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

func (r *Runner) Run(ctx context.Context, cases []Case) (Report, error) {
	report := Report{FailuresByRule: map[string]int{}}

	for _, c := range cases {
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

		result.Grades = GradeDraft(facts, draft)
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
			if err == nil {
				agreed := verdict.Approved() == result.Passed
				result.VerifierAgreed = &agreed
			}
		}

		report.Ran++
		if result.Passed {
			report.PassedCount++
		}
		report.Results = append(report.Results, result)
	}

	return report, nil
}
