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

	completer, model, err := agent.Provider{
		Kind:            cfg.ModelProvider,
		AnthropicAPIKey: cfg.AnthropicAPIKey,
		AnthropicModel:  cfg.AnthropicModel,
		BedrockModelID:  cfg.BedrockModelID,
		BedrockClient:   awsx.Bedrock(awsCfg),
	}.Build(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "running %d case(s) against %s, stopping at $%.2f\n", len(cases), model, *maxCost)

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
	facts = facts.WithPrecedent(agent.NewRetriever(pool, embedder, cfg.PrecedentLimit))

	var verifier *agent.Verifier
	if *withVerify {
		verifier = agent.NewVerifier(completer, model, pricing, 2048)
	}

	report, err := eval.NewRunner(pool, facts,
		agent.NewGenerator(completer, model, pricing, 4096), verifier).
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
	fmt.Fprintln(w, "CASE\tDISPUTE\tRESULT\tCOST\tPRECEDENT\tDETAIL")
	for _, r := range report.Results {
		found := fmt.Sprintf("%s %d", r.Retrieval, r.Precedents)
		switch {
		case r.Skipped:
			fmt.Fprintf(w, "%s\t-\tskipped\t-\t-\t%s\n", r.Case, r.Err)
		case r.Err != "":
			fmt.Fprintf(w, "%s\t%d\tERROR\t%s\t%s\t%s\n", r.Case, r.DisputeID, dollars(r.CostMicros), found, r.Err)
		case r.Passed:
			fmt.Fprintf(w, "%s\t%d\tpass\t%s\t%s\t\n", r.Case, r.DisputeID, dollars(r.CostMicros), found)
		default:
			fmt.Fprintf(w, "%s\t%d\tFAIL\t%s\t%s\t%s\n", r.Case, r.DisputeID, dollars(r.CostMicros), found, firstFailure(r))
		}
	}
	w.Flush()

	fmt.Printf("\n%d ran, %d passed (%.0f%%), %d skipped\n",
		report.Ran, report.PassedCount, 100*report.PassRate(), report.SkippedCount)
	fmt.Printf("cost %s total, %s per dispute\n",
		dollars(report.TotalCostMicros), dollars(report.MeanCostMicros()))

	if len(report.FailuresByRule) > 0 {
		fmt.Println("\nfailures by rule:")
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

// printLetters is separate from the table because a letter is a paragraph and
// a table row is a line, and forcing one into the other loses both.
func printLetters(report eval.Report) {
	for _, r := range report.Results {
		if r.Letter == "" {
			continue
		}
		fmt.Printf("\n%s\n%s\n%s (%s)\n\n%s\n",
			strings.Repeat("=", 72), r.Case, r.Recommendation,
			map[bool]string{true: "passed", false: "FAILED"}[r.Passed], r.Letter)
		for _, g := range r.Grades {
			if !g.Passed {
				fmt.Printf("  ✗ %s: %s\n", g.Rule, g.Detail)
			}
		}
	}
}

func firstFailure(r eval.Result) string {
	for _, g := range r.Grades {
		if !g.Passed {
			return g.Rule + ": " + g.Detail
		}
	}
	return ""
}

// Micro-dollars are the unit everywhere in this system; they are only turned
// into a currency string here, at the edge, for the same reason the dashboard
// divides exactly once.
func dollars(micros int64) string {
	return fmt.Sprintf("$%.4f", float64(micros)/1_000_000)
}
