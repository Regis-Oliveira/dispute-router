// Command agent drafts chargeback representments for review.
//
// It never sends anything. Every run ends with a dispute in draft_ready and a
// row in agent_runs; the transition to 'represented' belongs to whoever
// approves the draft, and there is no code path from here to it.
//
// Two modes:
//
//	agent -dispute 1234           one dispute, by id
//	agent                        drain a batch of candidates
//	agent -dispute 1234 -prompt  print what a model would be given, and stop
//
// Every run costs money: the model is the one dependency this repository
// cannot run locally, so unlike the rest of it there is no way to exercise
// this binary for free - which is what -dry-run is for: it shows what would be
// worked on and stops.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/internal/agent"
	"github.com/regisoliveira/dispute-router/internal/api"
	"github.com/regisoliveira/dispute-router/internal/awsx"
	"github.com/regisoliveira/dispute-router/internal/config"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	var (
		disputeID  = flag.Int64("dispute", 0, "draft for one dispute by id")
		dryRun     = flag.Bool("dry-run", false, "list what would be worked on and stop, without spending anything")
		batch      = flag.Int("batch", 0, "how many disputes to drain (default from AGENT_BATCH_SIZE)")
		showPrompt = flag.Bool("prompt", false, "print the record a model would be given for -dispute, and stop")
		timeout    = flag.Duration("timeout", 5*time.Minute, "wall clock ceiling for the whole invocation")
		report     = flag.Bool("report", false, "summarise every run in agent_runs and stop, without spending anything")
		traceFile  = flag.String("trace", "", "write a Go execution trace of this invocation to the file (go tool trace <file>)")
	)
	flag.Parse()

	if *traceFile != "" {
		stopTrace, err := startTrace(*traceFile)
		if err != nil {
			return err
		}
		defer stopTrace()
	}

	cfg, err := config.Load()
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

	store := api.NewStore(pool)
	runs := agent.NewRuns(pool)

	if *report {
		return printReport(ctx, pool, os.Stdout)
	}

	if *batch <= 0 {
		*batch = cfg.AgentBatchSize
	}

	// Nothing below this line is built in a dry run, so a dry run works with no
	// AWS credentials and no model id at all. That is the point of it: the
	// question "what would this touch" should be answerable without being in a
	// position to touch anything.
	if *dryRun {
		return dry(ctx, logger, runs, *disputeID, cfg.AgentMaxAttempts, *batch)
	}

	awsCfg, err := awsx.Load(ctx, cfg.AWS())
	if err != nil {
		return err
	}

	evidence := awsx.S3(awsCfg)

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

	facts, err := agent.NewFactSource(store,
		api.NewEvidence(evidence, cfg.S3EvidenceBucket, time.Minute),
		agent.FactSourceOptions{
			Precedent: agent.NewRetriever(pool, embedder, cfg.PrecedentLimit),
			// Available for every dispute, unlike precedent, which needs a
			// claim to match on and therefore covers about one in seven.
			BaseRates: pool,
		})
	if err != nil {
		return err
	}

	// Printing the record spends nothing and is the fastest way to answer the
	// question that actually comes up: not "what did the model say" but "what
	// was it looking at". Retrieval, quarantine and truncation are all visible
	// here, and none of them are visible in a draft.
	if *showPrompt {
		if *disputeID <= 0 {
			return fmt.Errorf("-prompt needs -dispute")
		}
		record, err := facts.For(ctx, *disputeID)
		if err != nil {
			return err
		}
		rendered, err := record.Render()
		if err != nil {
			return err
		}
		fmt.Println(rendered)
		logger.Info("record built", "dispute", *disputeID,
			"retrieval", record.Retrieval.Method, "precedents", len(record.Precedents))
		return nil
	}

	completer, err := agent.NewAnthropic(cfg.AnthropicAPIKey, cfg.AnthropicModel)
	if err != nil {
		return err
	}
	model := cfg.AnthropicModel

	pricing := agent.Pricing{
		InputMicrosPerMTok:  cfg.AgentInputPerMTok,
		OutputMicrosPerMTok: cfg.AgentOutputPerMTok,
	}

	assistant := agent.NewAssistant(
		facts,
		agent.NewGenerator(completer, model, pricing, 4096),
		agent.NewVerifier(completer, model, pricing, 2048),
		runs,
		agent.AssistantOptions{
			Model:         model,
			MaxCostMicros: cfg.AgentMaxCostMicros,
			MaxAttempts:   cfg.AgentMaxAttempts,
			Logger:        logger,
		},
	)

	if *disputeID > 0 {
		outcome, err := assistant.Assist(ctx, *disputeID)
		if err != nil {
			return err
		}
		logger.Info("done", "dispute", *disputeID, "outcome", outcome)
		return nil
	}

	return drain(ctx, logger, assistant, runs, cfg.AgentMaxAttempts, *batch)
}

func dry(ctx context.Context, logger *slog.Logger, runs *agent.Runs, disputeID int64, maxAttempts, batch int) error {
	if disputeID > 0 {
		logger.Info("dry run", "would_draft_for", disputeID)
		return nil
	}
	ids, err := runs.Candidates(ctx, maxAttempts, batch)
	if err != nil {
		return err
	}
	logger.Info("dry run", "candidates", len(ids), "ids", ids)
	return nil
}

func drain(ctx context.Context, logger *slog.Logger, assistant *agent.Assistant, runs *agent.Runs, maxAttempts, batch int) error {
	ids, err := runs.Candidates(ctx, maxAttempts, batch)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		logger.Info("nothing to draft for")
		return nil
	}

	// Sequential on purpose. These calls cost money and the point of a batch is
	// to bound the spend, which parallelism would only make harder to reason
	// about - and the ceiling that matters is per dispute, not per second.
	var drafted, other int
	for _, id := range ids {
		outcome, err := assistant.Assist(ctx, id)
		switch {
		case errors.Is(err, agent.ErrClaimLost):
			// Somebody else took it between the listing and the hold. Expected
			// under any concurrency, and not worth stopping a batch for.
			logger.Debug("skipped, already held", "dispute", id)
			continue
		case err != nil:
			// One dispute failing is not the batch failing. It is logged,
			// released by Assist, and the rest continue.
			logger.Error("could not assist", "dispute", id, "error", err)
			other++
			continue
		}
		if outcome == agent.OutcomeDrafted {
			drafted++
		} else {
			other++
		}
		if ctx.Err() != nil {
			break
		}
	}

	logger.Info("batch done", "considered", len(ids), "drafted", drafted, "other", other)
	return nil
}
