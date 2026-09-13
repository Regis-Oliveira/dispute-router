// Command embed backfills the claim embeddings that precedent retrieval uses.
//
// Resumable: it asks the database what is missing, so stopping it and starting
// it again costs nothing and repeats nothing. Run it with -n first - that is
// free and tells you how many rows and roughly how much.
//
// Only settled disputes with a claim are embedded. An open dispute has no
// outcome and can never be precedent, so embedding one is paying to index a row
// the retrieval query filters out.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/cmd/internal/boot"
	"github.com/regisoliveira/dispute-router/internal/agent"
	"github.com/regisoliveira/dispute-router/internal/config"
)

func main() {
	logger := boot.Logger(false)
	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	var (
		dryRun    = flag.Bool("n", false, "report what is outstanding and stop, without spending anything")
		limit     = flag.Int("limit", 0, "embed at most this many rows (0 = everything outstanding)")
		batchSize = flag.Int("batch", 128, "rows per request")
		timeout   = flag.Duration("timeout", 30*time.Minute, "wall clock ceiling")
	)
	flag.Parse()

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

	// Counted before the embedder is built, so -n needs no key. How much is
	// outstanding is a question about the database, and the answer costs
	// nothing - charging a credential for it would be charging for the one
	// question somebody asks precisely to decide whether to spend.
	pending, err := agent.PendingEmbeddings(ctx, pool, cfg.VoyageModel)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%d claims outstanding for %s\n", pending, cfg.VoyageModel)

	if *dryRun || pending == 0 {
		return nil
	}

	embedder, err := agent.NewVoyage(cfg.VoyageAPIKey, cfg.VoyageModel)
	if err != nil {
		return err
	}
	backfill := agent.NewBackfill(pool, embedder, logger)

	stats, err := backfill.Run(ctx, *batchSize, *limit)
	// Reported either way: a run that stopped halfway still embedded what it
	// embedded, and the number is what tells you it is safe to just run it
	// again.
	fmt.Fprintf(os.Stderr, "embedded %d in %d batch(es) over %s, %d still outstanding\n",
		stats.Embedded, stats.Batches, stats.Elapsed.Round(time.Millisecond), stats.Remaining)
	return err
}
