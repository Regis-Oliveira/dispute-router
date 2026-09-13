package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Precedent is a settled dispute that resembles the one being drafted.
//
// The outcome is the payload. "A dispute like this was won here before" is a
// different sentence from "a dispute like this exists", and only the first is
// worth putting in front of a drafter.
type Precedent struct {
	Reference   string `json:"reference"`
	ReasonCode  string `json:"reason_code"`
	Kind        string `json:"kind"`
	Outcome     string `json:"outcome"`
	Claim       string `json:"cardholder_claim,omitempty"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`

	// How it was found, and how close. Carried into the prompt so the drafter
	// can weigh a near match differently from a distant one - and carried into
	// the trace so a bad draft can be traced to a bad neighbour.
	Method     string  `json:"method"`
	Similarity float64 `json:"similarity"`
}

// Retrieval is how precedents were found for one draft, recorded so that a
// change in draft quality can be attributed to a change in retrieval.
type Retrieval struct {
	Method string `json:"method"`
	Model  string `json:"model,omitempty"`
	Found  int    `json:"found"`
	Note   string `json:"note,omitempty"`
}

// Retriever finds precedent for a dispute.
//
// Two strategies, and both are real. The vector path needs an Embedder and an
// embedded corpus; the lexical path needs neither and is the baseline the
// vector path has to beat. A retriever with only the first cannot answer
// whether embeddings earned their cost, which for short text in one language
// is a genuinely open question.
type Retriever struct {
	pool     *pgxpool.Pool
	embedder Embedder
	limit    int
}

// NewRetriever binds a retriever to a pool; a nil embedder means lexical only,
// and limit at or below zero means 3.
func NewRetriever(pool *pgxpool.Pool, embedder Embedder, limit int) *Retriever {
	if limit <= 0 {
		limit = 3
	}
	return &Retriever{pool: pool, embedder: embedder, limit: limit}
}

// For finds precedent for one dispute.
//
// Scoped to the same merchant on purpose. A won argument at one merchant is
// weak evidence at another - different descriptors, different policies,
// different acquirer - and a precedent block full of strangers teaches the
// drafter to write in generalities.
func (r *Retriever) For(ctx context.Context, disputeID int64, merchantExternalID, claim string) ([]Precedent, Retrieval, error) {
	if strings.TrimSpace(claim) == "" {
		// Nothing to match on. Most cardholders file through their bank and
		// say nothing, so this is the common case, not the edge one.
		return nil, Retrieval{Method: "none", Note: "no cardholder claim to match against"}, nil
	}

	if r.embedder != nil {
		precedents, err := r.byVector(ctx, disputeID, merchantExternalID, claim)
		if err != nil {
			return nil, Retrieval{}, err
		}
		if len(precedents) > 0 {
			return precedents, Retrieval{
				Method: "vector", Model: r.embedder.Model(), Found: len(precedents),
			}, nil
		}
		// An embedder with an empty corpus is a configuration state, not an
		// answer. Falling through to lexical is better than returning nothing
		// and letting the draft look like there is no precedent.
		precedents, err = r.Lexical(ctx, disputeID, merchantExternalID, claim)
		if err != nil {
			return nil, Retrieval{}, err
		}
		return precedents, Retrieval{
			Method: "lexical", Found: len(precedents),
			Note: "no embedded neighbours; corpus may not be backfilled",
		}, nil
	}

	precedents, err := r.Lexical(ctx, disputeID, merchantExternalID, claim)
	if err != nil {
		return nil, Retrieval{}, err
	}
	return precedents, Retrieval{Method: "lexical", Found: len(precedents)}, nil
}

const precedentColumns = `
	d.external_id, d.reason_code, d.kind, d.state,
	d.cardholder_claim, d.amount_minor, d.currency`

// byVector is nearest-neighbour over the embedded claims.
//
// The query is embedded as a query, not as a document. Most embedding models
// are asymmetric, and using the document prefix for a search costs recall
// quietly: the results stay plausible and get worse, which is the hardest kind
// of regression to see.
func (r *Retriever) byVector(ctx context.Context, disputeID int64, merchant, claim string) ([]Precedent, error) {
	vectors, err := r.embedder.Embed(ctx, []string{claim}, EmbedQuery)
	if err != nil {
		return nil, fmt.Errorf("embedding the claim: %w", err)
	}
	return r.NearestTo(ctx, disputeID, merchant, vectors[0])
}

// NearestTo searches with a query that is already embedded.
//
// Split out from byVector because turning text into a vector and searching with
// it are different operations with different costs, and joining them forces one
// API request per search. Anything comparing or batching - the retrieval
// comparator, a run over a queue of disputes - embeds every query in one request
// and then searches locally, which on a rate-limited account is the difference
// between working and not.
func (r *Retriever) NearestTo(ctx context.Context, disputeID int64, merchant string, query []float32) ([]Precedent, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+precedentColumns+`,
		       1 - (e.embedding <=> $1::vector) AS similarity
		  FROM dispute_embeddings e
		  JOIN disputes  d ON d.id = e.dispute_id
		  JOIN merchants m ON m.id = d.merchant_id
		 WHERE m.external_id = $2
		   AND d.state IN ('won', 'lost')
		   AND d.id <> $3
		   AND e.model = $4
		 ORDER BY e.embedding <=> $1::vector
		 LIMIT $5`,
		pgvector(query), merchant, disputeID, r.embedder.Model(), r.limit)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}
	defer rows.Close()

	return scanPrecedents(rows, "vector")
}

// orQuery turns a whole claim into a tsquery joined by OR.
//
// The first version used websearch_to_tsquery on the entire sentence, which ANDs
// every term: a candidate had to contain every word of the claim being searched.
// That is not a baseline, it is a straw man - and a vector index compared
// against it would have looked good for the wrong reason. This is the mistake
// the comparison exists to catch, and it caught it in its own first run.
//
// OR with ts_rank is the honest version: any shared term makes a candidate
// eligible and the ranking decides. Built in SQL so the tokenizer producing the
// query is the same one that produced claim_tsv - splitting on whitespace in Go
// would disagree with Postgres about what a word is.
const orQuery = `nullif(array_to_string(tsvector_to_array(to_tsvector('simple', $1)), ' | '), '')::tsquery`

// Lexical is the baseline: full-text ranking over the same claims. Exported
// for the same reason as NearestTo: a comparison needs to run both halves side
// by side.
func (r *Retriever) Lexical(ctx context.Context, disputeID int64, merchant, claim string) ([]Precedent, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+precedentColumns+`,
		       ts_rank(d.claim_tsv, `+orQuery+`) AS similarity
		  FROM disputes  d
		  JOIN merchants m ON m.id = d.merchant_id
		 WHERE m.external_id = $2
		   AND d.state IN ('won', 'lost')
		   AND d.id <> $3
		   AND d.claim_tsv @@ `+orQuery+`
		 ORDER BY similarity DESC
		 LIMIT $4`,
		claim, merchant, disputeID, r.limit)
	if err != nil {
		return nil, fmt.Errorf("text search: %w", err)
	}
	defer rows.Close()

	return scanPrecedents(rows, "lexical")
}

type scannable interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanPrecedents(rows scannable, method string) ([]Precedent, error) {
	out := []Precedent{}
	for rows.Next() {
		var p Precedent
		if err := rows.Scan(&p.Reference, &p.ReasonCode, &p.Kind, &p.Outcome,
			&p.Claim, &p.AmountMinor, &p.Currency, &p.Similarity); err != nil {
			return nil, fmt.Errorf("scan precedent: %w", err)
		}
		p.Method = method
		out = append(out, p)
	}
	return out, rows.Err()
}
