package agent

import (
	"context"
	"log/slog"
	"os"
	"testing"
)

// These run against the seeded dataset, which has real settled disputes with
// real claims. The fake embedder makes the vector path exercisable without
// spending anything; what it cannot show is whether embeddings retrieve BETTER
// than the baseline, which is a property of the model.
func liveRetrieval(t *testing.T) (*pgxpoolHandle, string, string) {
	t.Helper()
	store, pool := liveStore(t)
	_ = store

	var merchant, claim string
	err := pool.QueryRow(context.Background(), `
		SELECT m.external_id, d.cardholder_claim
		  FROM disputes d JOIN merchants m ON m.id = d.merchant_id
		 WHERE d.state IN ('won','lost') AND d.cardholder_claim <> ''
		 ORDER BY d.id LIMIT 1`).Scan(&merchant, &claim)
	if err != nil {
		t.Skipf("no settled dispute with a claim: %v", err)
	}
	return &pgxpoolHandle{pool}, merchant, claim
}

// The baseline finds real neighbours in the real dataset. If this returns
// nothing, the vector path has nothing to beat and the comparison is empty.
func TestLexicalRetrievalFindsRealPrecedent(t *testing.T) {
	handle, merchant, claim := liveRetrieval(t)

	precedents, retrieval, err := NewRetriever(handle.pool, nil, 3).
		For(context.Background(), -1, merchant, claim)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if retrieval.Method != "lexical" {
		t.Fatalf("method = %q", retrieval.Method)
	}
	if len(precedents) == 0 {
		t.Fatal("no precedent found for a claim drawn from the dataset itself")
	}

	for _, p := range precedents {
		if p.Outcome != "won" && p.Outcome != "lost" {
			t.Errorf("precedent %s is in state %q; only settled disputes are precedent",
				p.Reference, p.Outcome)
		}
	}
	t.Logf("lexical: %d precedent(s), top similarity %.4f, outcome %s",
		len(precedents), precedents[0].Similarity, precedents[0].Outcome)
}

// The vector path end to end: embed a corpus, search it, get neighbours back in
// distance order.
func TestVectorRetrievalRoundTrips(t *testing.T) {
	handle, merchant, claim := liveRetrieval(t)
	ctx := context.Background()
	embedder := &wordVector{dims: 1024}

	// A small backfill, because this is about the round trip and not the size.
	stats, err := NewBackfill(handle.pool, embedder,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))).
		Run(ctx, 64, 200)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if stats.Embedded == 0 {
		t.Skip("nothing to embed")
	}
	t.Cleanup(func() {
		_, _ = handle.pool.Exec(context.Background(),
			"DELETE FROM dispute_embeddings WHERE model = $1", embedder.Model())
	})

	precedents, retrieval, err := NewRetriever(handle.pool, embedder, 3).
		For(ctx, -1, merchant, claim)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	t.Logf("vector: method=%s found=%d", retrieval.Method, len(precedents))

	if retrieval.Method == "vector" {
		// Neighbours come back nearest first, and similarity is a cosine so it
		// cannot leave [-1, 1]. A value outside it means the distance operator
		// and the similarity expression disagree.
		for i, p := range precedents {
			if p.Similarity < -1.0001 || p.Similarity > 1.0001 {
				t.Errorf("precedent %d similarity %.4f is outside cosine range", i, p.Similarity)
			}
			if i > 0 && p.Similarity > precedents[i-1].Similarity+1e-6 {
				t.Errorf("precedent %d is closer than %d; results are not in distance order", i, i-1)
			}
		}
	}
}

// The property that makes the backfill safe to run: it asks what is missing, so
// running it twice does not embed anything twice.
func TestTheBackfillIsResumable(t *testing.T) {
	handle, _, _ := liveRetrieval(t)
	ctx := context.Background()
	embedder := &wordVector{dims: 1024}
	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	backfill := NewBackfill(handle.pool, embedder, quiet)
	t.Cleanup(func() {
		_, _ = handle.pool.Exec(context.Background(),
			"DELETE FROM dispute_embeddings WHERE model = $1", embedder.Model())
	})

	before, err := backfill.Pending(ctx)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if before < 20 {
		t.Skip("not enough claims to exercise a partial backfill")
	}

	first, err := backfill.Run(ctx, 8, 16)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if first.Embedded != 16 {
		t.Fatalf("embedded %d, asked for 16", first.Embedded)
	}
	if first.Remaining != before-16 {
		t.Errorf("remaining = %d, want %d", first.Remaining, before-16)
	}

	second, err := backfill.Run(ctx, 8, 16)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.Remaining != before-32 {
		t.Errorf("the second run repeated work: remaining = %d, want %d", second.Remaining, before-32)
	}
}
