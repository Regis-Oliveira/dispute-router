// Command eval measures the representment assistant against a fixed set of
// disputes.
//
// It reads. It never holds a dispute, moves one, or writes an agent_run:
// measuring a system is not using it, and a benchmark that leaves rows behind
// changes what the next run will find.
//
// There are two things worth measuring and they need different runs.
//
// The graders in internal/agent/eval are pure functions and have their own
// tests, which run in CI for nothing. That answers "is the harness correct".
//
// This binary answers the other question - "is the model good enough" - and
// there is no free way to ask it. Every case is a real call. -cases prints the
// set and the disputes it would select without spending anything.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/internal/agent"
	"github.com/regisoliveira/dispute-router/internal/agent/eval"
	"github.com/regisoliveira/dispute-router/internal/api"
	"github.com/regisoliveira/dispute-router/internal/awsx"
	"github.com/regisoliveira/dispute-router/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listOnly   = flag.Bool("cases", false, "print the cases and the disputes they select, and stop")
		withVerify = flag.Bool("verify", false, "also run the verifier, and report where it disagrees with the graders")
		asJSON     = flag.Bool("json", false, "emit the full report as JSON")
		showLetter = flag.Bool("letters", false, "print each draft under its result")
		only       = flag.String("only", "", "run one case by name")
		samples    = flag.Int("samples", 1, "draft each case this many times. Cost is linear in it, and one sample cannot tell a fix from luck")
		maxCost    = flag.Float64("max-cost", 1.0, "stop the run once it has spent this many dollars")
		timeout    = flag.Duration("timeout", 10*time.Minute, "wall clock ceiling")
	)
	flag.Parse()

	cfg, err := config.Load(os.Getenv("DOTENV_PATH"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	cases := eval.Cases()
	if *only != "" {
		cases = filter(cases, *only)
		if len(cases) == 0 {
			return fmt.Errorf("no case named %q", *only)
		}
	}

	if *listOnly {
		return list(ctx, pool, cases)
	}

	awsCfg, err := awsx.Load(ctx, awsx.Config{
		Region:          cfg.AWSRegion,
		AccessKeyID:     cfg.AWSAccessKey,
		SecretAccessKey: cfg.AWSSecretKey,
	})
	if err != nil {
		return err
	}

	completer, err := agent.NewAnthropic(cfg.AnthropicAPIKey, cfg.AnthropicModel)
	if err != nil {
		return err
	}
	model := cfg.AnthropicModel
	fmt.Fprintf(os.Stderr, "running %d case(s) x %d sample(s) against %s, stopping at $%.2f\n",
		len(cases), *samples, model, *maxCost)

	pricing := agent.Pricing{
		InputMicrosPerMTok:  cfg.AgentInputPerMTok,
		OutputMicrosPerMTok: cfg.AgentOutputPerMTok,
	}
	facts := agent.NewFactSource(api.NewStore(pool),
		api.NewEvidence(awsx.S3(awsCfg, cfg.AWSEndpoint), cfg.S3EvidenceBucket, time.Minute))

	// Precedent retrieval. The embedder is optional: without a key the
	// retriever uses full-text search, which is the baseline anyway.
	var embedder agent.Embedder
	if cfg.VoyageAPIKey != "" {
		voyage, err := agent.NewVoyage(cfg.VoyageAPIKey, cfg.VoyageModel)
		if err != nil {
			return err
		}
		embedder = voyage
	}
	facts = facts.
		WithPrecedent(agent.NewRetriever(pool, embedder, cfg.PrecedentLimit)).
		// Available for every dispute, unlike precedent, which needs a claim to
		// match on and therefore covers about one in seven.
		WithBaseRates(pool)

	var verifier *agent.Verifier
	if *withVerify {
		verifier = agent.NewVerifier(completer, model, pricing, 2048)
	}

	report, err := eval.NewRunner(pool, facts,
		agent.NewGenerator(completer, model, pricing, 4096), verifier).
		WithSamples(*samples).
		WithCostCeiling(int64(*maxCost*1_000_000)).
		Run(ctx, cases)
	if err != nil {
		return err
	}

	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	print(report)
	if *showLetter {
		printLetters(report)
	}
	return nil
}

func filter(cases []eval.Case, name string) []eval.Case {
	var out []eval.Case
	for _, c := range cases {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// list resolves each case to a dispute without calling a model, so the question
// "what would this measure" is answerable for free - and so a case that no
// longer matches anything is visible before a paid run discovers it.
func list(ctx context.Context, pool *pgxpool.Pool, cases []eval.Case) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CASE\tDISPUTE\tWHY")
	for _, c := range cases {
		var id int64
		err := pool.QueryRow(ctx, c.Query).Scan(&id)
		switch {
		case err == pgx.ErrNoRows:
			fmt.Fprintf(w, "%s\t-\t%s\n", c.Name, "NO MATCHING DISPUTE: "+c.Why)
		case err != nil:
			fmt.Fprintf(w, "%s\tERROR\t%v\n", c.Name, err)
		default:
			fmt.Fprintf(w, "%s\t%d\t%s\n", c.Name, id, c.Why)
		}
	}
	return w.Flush()
}

func print(report eval.Report) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CASE\tDISPUTE\tPASSED\tVERIFIER\tCOST\tPRECEDENT\tFAILURES")
	for _, r := range report.Results {
		if r.Skipped {
			fmt.Fprintf(w, "%s\t-\tskipped\t-\t-\t-\t%s\n", r.Case, r.Err)
			continue
		}
		passed := fmt.Sprintf("%d/%d", r.PassedCount(), len(r.Samples))
		if r.Unstable() {
			// Marked, because a case that passes some samples and fails others
			// is the finding a single run cannot produce.
			passed += " !"
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s %d\t%s\n",
			r.Case, r.DisputeID, passed, verifierSummary(r), dollars(r.CostMicros),
			r.Retrieval, r.Precedents, failureSummary(r))
	}
	w.Flush()

	fmt.Printf("\n%d case(s) x %d sample(s) = %d run(s), %d passed (%.0f%%), %d skipped\n",
		report.Cases, report.Samples, report.Runs, report.PassedRuns,
		100*report.PassRate(), report.SkippedCount)

	// Said before the cost, because it changes how every other number here
	// should be read.
	switch {
	case report.Samples <= 1:
		fmt.Println("one sample per case: this cannot tell a fix from luck. Use -samples to find out.")
	case report.UnstableCases > 0:
		fmt.Printf("%d case(s) marked ! passed some samples and failed others; "+
			"any single-sample figure about them is a coin toss\n", report.UnstableCases)
	default:
		fmt.Printf("no case split its samples, so these results held across %d run(s) each\n", report.Samples)
	}

	if report.Stopped != "" {
		fmt.Printf("STOPPED EARLY: %s\n", report.Stopped)
	}

	// Where the verifier and the graders disagree is the reason -verify exists,
	// and the two directions mean different things: a draft the graders passed
	// and the verifier refused is something a pattern could not see; the
	// reverse is a rule the verifier let through.
	if missed, lenient, ran := verifierDisagreements(report); ran > 0 {
		fmt.Printf("verifier ran on %d run(s): refused %d the graders passed, passed %d the graders failed\n",
			ran, missed, lenient)
	}

	fmt.Printf("cost %s total, %s per run\n",
		dollars(report.TotalCostMicros), dollars(report.MeanCostMicros()))

	// Cache hits are reported whether or not there were any. A prompt too short
	// to cache and a cache working perfectly both produce a plausible cost, and
	// only these numbers say which happened.
	u := report.Usage
	cacheable := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	fmt.Printf("tokens %d in / %d out - cache %d written, %d read",
		u.InputTokens, u.OutputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens)
	if cacheable > 0 {
		fmt.Printf(" (%.0f%% of input served from cache)",
			100*float64(u.CacheReadInputTokens)/float64(cacheable))
	}
	fmt.Println()

	if len(report.FailuresByRule) > 0 {
		fmt.Println("\nfailures by rule, across all runs:")
		rules := make([]string, 0, len(report.FailuresByRule))
		for rule := range report.FailuresByRule {
			rules = append(rules, rule)
		}
		sort.Slice(rules, func(i, j int) bool {
			return report.FailuresByRule[rules[i]] > report.FailuresByRule[rules[j]]
		})
		for _, rule := range rules {
			fmt.Printf("  %-34s %d\n", rule, report.FailuresByRule[rule])
		}
	}
}

// verifierSummary is "agreed/ran" for one case, or "-" when no verifier ran.
func verifierSummary(r eval.Result) string {
	agreed, ran := 0, 0
	for _, s := range r.Samples {
		if s.VerifierAgreed == nil {
			continue
		}
		ran++
		if *s.VerifierAgreed {
			agreed++
		}
	}
	if ran == 0 {
		return "-"
	}
	return fmt.Sprintf("agreed %d/%d", agreed, ran)
}

func verifierDisagreements(report eval.Report) (refusedPassed, passedFailed, ran int) {
	for _, r := range report.Results {
		for _, s := range r.Samples {
			if s.VerifierApproved == nil {
				continue
			}
			ran++
			switch {
			case s.Passed && !*s.VerifierApproved:
				refusedPassed++
			case !s.Passed && *s.VerifierApproved:
				passedFailed++
			}
		}
	}
	return refusedPassed, passedFailed, ran
}

// failureSummary names the rules a case broke and how often. "3/5" says a case
// is unstable and not what it is unstable about.
func failureSummary(r eval.Result) string {
	byRule := r.FailuresByRule()
	if len(byRule) == 0 {
		return ""
	}
	rules := make([]string, 0, len(byRule))
	for rule := range byRule {
		rules = append(rules, rule)
	}
	sort.Slice(rules, func(i, j int) bool { return byRule[rules[i]] > byRule[rules[j]] })

	parts := make([]string, 0, len(rules))
	for _, rule := range rules {
		if n := byRule[rule]; n > 1 {
			parts = append(parts, fmt.Sprintf("%s x%d", rule, n))
		} else {
			parts = append(parts, rule)
		}
	}
	return strings.Join(parts, ", ")
}

// printLetters is separate from the table because a letter is a paragraph and
// a table row is a line, and forcing one into the other loses both.
func printLetters(report eval.Report) {
	for _, r := range report.Results {
		for i, sample := range r.Samples {
			if sample.Letter == "" {
				continue
			}
			label := r.Case
			if len(r.Samples) > 1 {
				label = fmt.Sprintf("%s (sample %d of %d)", r.Case, i+1, len(r.Samples))
			}
			fmt.Printf("\n%s\n%s\n%s (%s)\n\n%s\n",
				strings.Repeat("=", 72), label, sample.Recommendation,
				map[bool]string{true: "passed", false: "FAILED"}[sample.Passed], sample.Letter)
			for _, g := range sample.Grades {
				if !g.Passed {
					fmt.Printf("  x %s: %s\n", g.Rule, g.Detail)
				}
			}
		}
	}
}

func dollars(micros int64) string {
	return fmt.Sprintf("$%.4f", float64(micros)/1_000_000)
}
