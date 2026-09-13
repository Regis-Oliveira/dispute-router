//go:build integration

package agent

import (
	"testing"

	"github.com/regisoliveira/dispute-router/internal/llm"
)

// The three retrieval tests that need a database of their own. Retrieval reads
// dispute_embeddings, and a search over the development database's rows would
// assert on whatever happened to be seeded there - so these run against the
// empty scratch database scratchDB creates and drops, and carry that file's
// integration tag.
//
// wordVector stays in precedent_test.go, untagged, because precedent_live_test.go
// uses it too and that file is in the default run: a helper both builds need
// cannot live behind the tag.
//
// The seven precedent tests that only render a Facts value are pure, and stay
// untagged beside it.

// The asymmetry most callers ignore: a stored document and a search query are
// embedded differently, and confusing them costs recall quietly.
func TestAQueryIsEmbeddedAsAQuery(t *testing.T) {
	pool := scratchDB(t)
	embedder := &wordVector{dims: 1024}
	retriever := NewRetriever(pool, embedder, 3)

	if _, _, err := retriever.For(t.Context(), 1, "mrc_nothing", "the parcel never arrived"); err != nil {
		t.Fatalf("For: %v", err)
	}
	if len(embedder.kinds) == 0 || embedder.kinds[0] != llm.EmbedQuery {
		t.Errorf("the claim was embedded as %v, want %q", embedder.kinds, llm.EmbedQuery)
	}
}

// Most cardholders file through their bank and say nothing. Retrieval has to
// say so rather than searching for the empty string, which matches everything.
func TestNoClaimMeansNoSearch(t *testing.T) {
	pool := scratchDB(t)
	embedder := &wordVector{dims: 1024}

	precedents, retrieval, err := NewRetriever(pool, embedder, 3).
		For(t.Context(), 1, "mrc_nothing", "   ")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if len(precedents) != 0 || retrieval.Method != RetrievalNone {
		t.Errorf("precedents=%d method=%q; an absent claim triggered a search",
			len(precedents), retrieval.Method)
	}
	if len(embedder.kinds) != 0 {
		t.Error("an embedding was paid for with nothing to embed")
	}
}

// Without an embedder the retriever is not disabled - it uses the baseline.
func TestWithoutAnEmbedderRetrievalIsLexical(t *testing.T) {
	pool := scratchDB(t)

	_, retrieval, err := NewRetriever(pool, nil, 3).
		For(t.Context(), 1, "mrc_nothing", "the parcel never arrived")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if retrieval.Method != RetrievalLexical {
		t.Errorf("method = %q, want lexical", retrieval.Method)
	}
}
