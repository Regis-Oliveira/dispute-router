package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Backfill embeds the claims that are not embedded yet.
//
// Resumable by construction: it asks for what is missing, so stopping it and
// starting it again costs nothing and repeats nothing. That is not a nicety -
// this is the only operation in the project that can spend real money across
// thousands of rows, and one that has to run to completion or start over is one
// nobody dares run.
type Backfill struct {
	pool     *pgxpool.Pool
	embedder Embedder
	log      *slog.Logger
}

// NewBackfill binds a backfill to a pool and an embedder; a nil log means the default.
func NewBackfill(pool *pgxpool.Pool, embedder Embedder, log *slog.Logger) *Backfill {
	if log == nil {
		log = slog.Default()
	}
	return &Backfill{pool: pool, embedder: embedder, log: log}
}

// BackfillStats is what one Run did and what it left outstanding.
type BackfillStats struct {
	Embedded  int
	Remaining int64
	Batches   int
	Elapsed   time.Duration
}

// PendingEmbeddings counts the claims still to embed for a model.
//
// A package function taking a model name rather than a method needing an
// Embedder, because "how much is outstanding" is a question about the database
// and should cost nothing to ask. Requiring a key to count rows is the same
// mistake as requiring one to print a prompt: the free question stops being
// free for no reason.
//
// Keyed on the model, because vectors from two models are not comparable.
// Changing model does not update rows, it makes every existing row the wrong
// answer to a query that filters by model - and this count is how that becomes
// visible rather than silently halving recall.
func PendingEmbeddings(ctx context.Context, pool *pgxpool.Pool, model string) (int64, error) {
	var n int64
	err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM disputes d
		 WHERE d.cardholder_claim <> ''
		   AND d.state IN ('won', 'lost')
		   AND NOT EXISTS (
		         SELECT 1 FROM dispute_embeddings e
		          WHERE e.dispute_id = d.id AND e.model = $1
		       )`, model).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting pending embeddings: %w", err)
	}
	return n, nil
}

// Pending counts the claims still to embed for this backfill's model.
func (b *Backfill) Pending(ctx context.Context) (int64, error) {
	return PendingEmbeddings(ctx, b.pool, b.embedder.Model())
}

// Run embeds up to limit claims. Zero means everything outstanding.
//
// Only settled disputes are embedded. An open dispute has no outcome, so it can
// never be precedent, and embedding it would be paying to index rows that the
// retrieval query filters out.
func (b *Backfill) Run(ctx context.Context, batchSize, limit int) (stats BackfillStats, err error) {
	started := time.Now()

	// The outstanding count is recomputed on every exit, including the failing
	// ones. It used to be set only on the happy path, so a run that stopped on
	// a rate limit reported "0 still outstanding" - the one number that would
	// make somebody stop, believing it had finished. A partial run is the
	// normal case here; its report has to be true.
	defer func() {
		stats.Elapsed = time.Since(started)
		remaining, countErr := b.Pending(context.WithoutCancel(ctx))
		if countErr == nil {
			stats.Remaining = remaining
		}
	}()
	if batchSize <= 0 || batchSize > voyageMaxBatch {
		batchSize = voyageMaxBatch
	}

	for {
		if limit > 0 && stats.Embedded >= limit {
			break
		}
		size := batchSize
		if limit > 0 && limit-stats.Embedded < size {
			size = limit - stats.Embedded
		}

		ids, claims, err := b.next(ctx, size)
		if err != nil {
			return stats, err
		}
		if len(ids) == 0 {
			break
		}

		vectors, err := b.embedder.Embed(ctx, claims, EmbedDocument)
		if err != nil {
			// The batches already written stay written. That is the point of
			// keying on what is missing rather than tracking a cursor.
			return stats, fmt.Errorf("after %d embedded: %w", stats.Embedded, err)
		}

		if err := b.write(ctx, ids, vectors); err != nil {
			return stats, err
		}

		stats.Embedded += len(ids)
		stats.Batches++
		b.log.Info("embedded", "batch", stats.Batches, "rows", len(ids), "total", stats.Embedded)

		if ctx.Err() != nil {
			// A run that was cut short is not a run that finished. Returning nil
			// here made cmd/embed print a success for a backfill that had been
			// interrupted, with the stats above as the only hint.
			return stats, ctx.Err()
		}
	}

	return stats, nil
}

func (b *Backfill) next(ctx context.Context, size int) ([]int64, []string, error) {
	rows, err := b.pool.Query(ctx, `
		SELECT d.id, d.cardholder_claim
		  FROM disputes d
		 WHERE d.cardholder_claim <> ''
		   AND d.state IN ('won', 'lost')
		   AND NOT EXISTS (
		         SELECT 1 FROM dispute_embeddings e
		          WHERE e.dispute_id = d.id AND e.model = $1
		       )
		 ORDER BY d.id
		 LIMIT $2`, b.embedder.Model(), size)
	if err != nil {
		return nil, nil, fmt.Errorf("selecting claims to embed: %w", err)
	}
	defer rows.Close()

	var ids []int64
	var claims []string
	for rows.Next() {
		var id int64
		var claim string
		if err := rows.Scan(&id, &claim); err != nil {
			return nil, nil, fmt.Errorf("scan claim: %w", err)
		}
		ids = append(ids, id)
		claims = append(claims, claim)
	}
	return ids, claims, rows.Err()
}

func (b *Backfill) write(ctx context.Context, ids []int64, vectors [][]float32) error {
	if len(ids) != len(vectors) {
		return fmt.Errorf("embedding count %d does not match row count %d", len(vectors), len(ids))
	}

	batch := &pgx.Batch{}
	for i, id := range ids {
		batch.Queue(`
			INSERT INTO dispute_embeddings (dispute_id, embedding, model, source)
			VALUES ($1, $2::vector, $3, 'cardholder_claim')
			ON CONFLICT (dispute_id) DO UPDATE
			   SET embedding = EXCLUDED.embedding,
			       model     = EXCLUDED.model,
			       created_at = now()`,
			id, pgvector(vectors[i]), b.embedder.Model())
	}

	results := b.pool.SendBatch(ctx, batch)
	defer results.Close()
	for range ids {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("writing embeddings: %w", err)
		}
	}
	return nil
}
