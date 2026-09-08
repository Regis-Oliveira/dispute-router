// Command agent drafts chargeback representments for review.
//
// It never sends anything. Every run ends with a dispute in draft_ready and a
// row in agent_runs; the transition to 'represented' belongs to whoever
// approves the draft, and there is no code path from here to it.
//
// Two modes:
//
//	agent -dispute 1234    one dispute, by id
//	agent                  drain a batch of candidates
//
// Every run costs money. Bedrock is the one AWS service LocalStack does not
// emulate, so unlike the rest of this repository there is no way to exercise
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
		disputeID = flag.Int64("dispute", 0, "draft for one dispute by id")
		dryRun    = flag.Bool("dry-run", false, "list what would be worked on and stop, without spending anything")
		batch     = flag.Int("batch", 0, "how many disputes to drain (default from AGENT_BATCH_SIZE)")
		timeout   = flag.Duration("timeout", 5*time.Minute, "wall clock ceiling for the whole invocation")
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

	store := api.NewStore(pool)
	runs := agent.NewRuns(pool)

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

	awsCfg, err := awsx.Load(ctx, awsx.Config{
		Region:          cfg.AWSRegion,
		AccessKeyID:     cfg.AWSAccessKey,
		SecretAccessKey: cfg.AWSSecretKey,
	})
	if err != nil {
		return err
	}

	completer, err := agent.NewBedrock(awsx.Bedrock(awsCfg), cfg.BedrockModelID)
	if err != nil {
		return err
	}

	pricing := agent.Pricing{
		InputMicrosPerMTok:  cfg.AgentInputPerMTok,
		OutputMicrosPerMTok: cfg.AgentOutputPerMTok,
	}

	evidence := awsx.S3(awsCfg, cfg.AWSEndpoint)
	assistant := agent.NewAssistant(
		agent.NewFactSource(store, api.NewEvidence(evidence, cfg.S3EvidenceBucket, time.Minute)),
		agent.NewGenerator(completer, cfg.BedrockModelID, pricing, 4096),
		agent.NewVerifier(completer, cfg.BedrockModelID, pricing, 2048),
		runs,
		agent.AssistantOptions{
			Model:         cfg.BedrockModelID,
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
