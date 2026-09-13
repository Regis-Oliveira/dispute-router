// Command retrieval compares the precedent strategies on the same disputes.
//
// It exists to answer one question with numbers instead of a preference: does
// the vector index retrieve better than Postgres full-text search here? For
// short claims in one language, already filtered to one merchant and to settled
// disputes, that contest is genuinely open - and a vector index nobody has
// compared against the baseline is a claim rather than a result.
//
// The first version of this comparison was wrong twice, in opposite
// directions, and both are worth keeping in mind when reading its output. The
// lexical baseline ANDed every term of the claim, which found nothing in a
// quarter of cases and made the vector index look 23% better. Fixed to OR, it
// matched nearly every candidate - with the 'simple' configuration stop words
// are terms - so "found something" became identical by construction, and a
// headline that said "identical coverage" was reading a counter that could not
// move. The number that carries information is set overlap: which disputes
// each strategy returned. This version reports that first, says how often the
// lexical filter was a no-op, says how many candidates are exact-text ties
// (on a fifteen-sentence pool, most top-3s are), adds a stop-word-aware
// baseline beside the production one, and refuses to run unless every
// candidate the lexical search can see is also embedded - the last comparison
// nearly ran against a half-built index.
//
// Embedding the queries costs a little; everything else is local. It never
// writes.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/internal/agent"
	"github.com/regisoliveira/dispute-router/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type target struct {
	id       int64
	merchant string
	claim    string
}

type comparison struct {
	target  target
	vector  []agent.Precedent
	simple  []agent.Precedent // the production baseline: 'simple' config, OR of every term
	english []agent.Precedent // stop words dropped, stems matched

	// The population each strategy chose from, and how much of it the
	// lexical OR let through. When matched == candidates the filter did
	// nothing and ranking did all the work.
	candidates, matched int
	// How many candidates carry the target's exact claim text. Above k, the
	// top-k is a tie broken by whatever order the index returned.
	identical int
}

func run() error {
	var (
		sample       = flag.Int("n", 20, "how many disputes to compare")
		limit        = flag.Int("k", 3, "precedents per strategy")
		verbose      = flag.Bool("v", false, "print each disagreement")
		allowPartial = flag.Bool("allow-partial", false, "compare even if some lexical candidates are not embedded (the numbers will not mean what they say)")
		timeout      = flag.Duration("timeout", 10*time.Minute, "wall clock ceiling")
	)
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireDatabase(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	embedder, err := agent.NewVoyage(cfg.Agent.VoyageAPIKey, cfg.Agent.VoyageModel)
	if err != nil {
		return err
	}

	targets, err := loadTargets(ctx, pool, *sample)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("no open chargebacks with a claim to compare on")
	}

	// Pool parity first. A vector index that covers half the candidates the
	// lexical search can see loses half its overlap for a reason that has
	// nothing to do with retrieval, and the report would call it disagreement.
	settled, embedded, err := poolParity(ctx, pool, targets, embedder.Model())
	if err != nil {
		return err
	}
	if embedded != settled {
		msg := fmt.Sprintf("pool parity: %d settled claims among these merchants, %d embedded for %s",
			settled, embedded, embedder.Model())
		if !*allowPartial {
			return fmt.Errorf("%s; run cmd/embed to completion first, or pass -allow-partial to compare anyway", msg)
		}
		fmt.Fprintln(os.Stderr, "WARNING:", msg, "- overlap below is partly the index's coverage, not retrieval")
	}

	withVector := agent.NewRetriever(pool, embedder, *limit)
	withText := agent.NewRetriever(pool, nil, *limit)

	// Every query embedded in one request, then searched locally.
	claims := make([]string, len(targets))
	for i, t := range targets {
		claims[i] = t.claim
	}
	queries, err := embedder.Embed(ctx, claims, agent.EmbedQuery)
	if err != nil {
		return fmt.Errorf("embedding %d queries: %w", len(claims), err)
	}

	var results []comparison
	for i, t := range targets {
		c := comparison{target: t}
		if c.vector, err = withVector.NearestTo(ctx, t.id, t.merchant, queries[i]); err != nil {
			return fmt.Errorf("dispute %d vector: %w", t.id, err)
		}
		if c.simple, err = withText.Lexical(ctx, t.id, t.merchant, t.claim); err != nil {
			return fmt.Errorf("dispute %d lexical: %w", t.id, err)
		}
		if c.english, err = englishRanked(ctx, pool, t, *limit); err != nil {
			return fmt.Errorf("dispute %d english: %w", t.id, err)
		}
		if c.candidates, c.matched, c.identical, err = population(ctx, pool, t); err != nil {
			return fmt.Errorf("dispute %d population: %w", t.id, err)
		}
		results = append(results, c)
	}

	report(results, *limit, *verbose)
	return nil
}

func loadTargets(ctx context.Context, pool *pgxpool.Pool, n int) ([]target, error) {
	// Open disputes with a claim: the population the agent actually drafts for.
	rows, err := pool.Query(ctx, `
		SELECT d.id, m.external_id, d.cardholder_claim
		  FROM disputes d JOIN merchants m ON m.id = d.merchant_id
		 WHERE d.kind = 'chargeback' AND d.cardholder_claim <> ''
		   AND d.state = 'received' AND d.deadline_at > now()
		 ORDER BY d.id
		 LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.merchant, &t.claim); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// poolParity counts, over the merchants being compared, the settled claims the
// lexical search can reach and how many of them the vector index holds.
func poolParity(ctx context.Context, pool *pgxpool.Pool, targets []target, model string) (settled, embedded int, err error) {
	merchants := make([]string, 0, len(targets))
	seen := map[string]bool{}
	for _, t := range targets {
		if !seen[t.merchant] {
			seen[t.merchant] = true
			merchants = append(merchants, t.merchant)
		}
	}
	err = pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE EXISTS (
		         SELECT 1 FROM dispute_embeddings e WHERE e.dispute_id = d.id AND e.model = $2))
		  FROM disputes d JOIN merchants m ON m.id = d.merchant_id
		 WHERE m.external_id = ANY($1)
		   AND d.state IN ('won', 'lost') AND d.cardholder_claim <> ''`,
		merchants, model).Scan(&settled, &embedded)
	return settled, embedded, err
}

// population is what one target's lexical search chose from, and how much of
// it the OR query let through.
func population(ctx context.Context, pool *pgxpool.Pool, t target) (candidates, matched, identical int, err error) {
	err = pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE d.claim_tsv @@
		         nullif(array_to_string(tsvector_to_array(to_tsvector('simple', $3)), ' | '), '')::tsquery),
		       count(*) FILTER (WHERE d.cardholder_claim = $3)
		  FROM disputes d JOIN merchants m ON m.id = d.merchant_id
		 WHERE m.external_id = $1 AND d.state IN ('won', 'lost') AND d.id <> $2
		   AND d.cardholder_claim <> ''`,
		t.merchant, t.id, t.claim).Scan(&candidates, &matched, &identical)
	return
}

// englishRanked is the stop-word-aware baseline: the same OR query, but with
// the 'english' configuration on both sides, so "I", "the" and "my" are not
// terms and "delivered" matches "delivery". Computed on the fly - there is no
// index for it, and there are a few hundred candidates - because it exists to
// answer whether the production baseline's filter was a no-op, not to ship.
func englishRanked(ctx context.Context, pool *pgxpool.Pool, t target, k int) ([]agent.Precedent, error) {
	const q = `nullif(array_to_string(tsvector_to_array(to_tsvector('english', $1)), ' | '), '')::tsquery`
	rows, err := pool.Query(ctx, `
		SELECT d.external_id, d.reason_code, d.kind, d.state, d.cardholder_claim,
		       d.amount_minor, d.currency,
		       ts_rank(to_tsvector('english', d.cardholder_claim), `+q+`) AS similarity
		  FROM disputes d JOIN merchants m ON m.id = d.merchant_id
		 WHERE m.external_id = $2 AND d.state IN ('won', 'lost') AND d.id <> $3
		   AND to_tsvector('english', d.cardholder_claim) @@ `+q+`
		 ORDER BY similarity DESC
		 LIMIT $4`, t.claim, t.merchant, t.id, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agent.Precedent
	for rows.Next() {
		var p agent.Precedent
		if err := rows.Scan(&p.Reference, &p.ReasonCode, &p.Kind, &p.Outcome, &p.Claim,
			&p.AmountMinor, &p.Currency, &p.Similarity); err != nil {
			return nil, err
		}
		p.Method = "english"
		out = append(out, p)
	}
	return out, rows.Err()
}

func overlapOf(a, b []agent.Precedent) int {
	seen := map[string]bool{}
	for _, p := range a {
		seen[p.Reference] = true
	}
	n := 0
	for _, p := range b {
		if seen[p.Reference] {
			n++
		}
	}
	return n
}

// report says what the strategies did differently, and deliberately does not
// declare a winner: nobody has labelled which precedent was the RIGHT one to
// retrieve, so overlap is disagreement, not quality. What it can say honestly
// is how different the returned sets are, whether the lexical filter filtered
// anything, and how much of the disagreement is tie-breaking among identical
// texts.
func report(results []comparison, k int, verbose bool) {
	var (
		identical, partial, disjoint       int
		vectorEmpty, simpleEmpty, engEmpty int
		filterNoop, tiedTopK               int
		sumVS, sumVE, sumSE                int
	)
	for _, c := range results {
		o := overlapOf(c.vector, c.simple)
		switch {
		case len(c.vector) == 0 || len(c.simple) == 0:
		case o == len(c.vector) && o == len(c.simple):
			identical++
		case o == 0:
			disjoint++
		default:
			partial++
		}
		if len(c.vector) == 0 {
			vectorEmpty++
		}
		if len(c.simple) == 0 {
			simpleEmpty++
		}
		if len(c.english) == 0 {
			engEmpty++
		}
		if c.candidates > 0 && c.matched == c.candidates {
			filterNoop++
		}
		if c.identical >= k {
			tiedTopK++
		}
		sumVS += o
		sumVE += overlapOf(c.vector, c.english)
		sumSE += overlapOf(c.simple, c.english)
	}
	n := len(results)
	mean := func(sum int) string { return fmt.Sprintf("%.2f of %d", float64(sum)/float64(max(1, n)), k) }

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "disputes compared\t%d\ttop-%d each\n", n, k)
	fmt.Fprintf(w, "\nSET OVERLAP, vector vs lexical (simple)\t\t\n")
	fmt.Fprintf(w, "  identical sets\t%d\t%s\n", identical, pct(identical, n))
	fmt.Fprintf(w, "  partial overlap\t%d\t%s\n", partial, pct(partial, n))
	fmt.Fprintf(w, "  disjoint sets\t%d\t%s\n", disjoint, pct(disjoint, n))
	fmt.Fprintf(w, "  mean overlap\t%s\t\n", mean(sumVS))
	fmt.Fprintf(w, "\nWHY THE SETS DIFFER\t\t\n")
	fmt.Fprintf(w, "  lexical OR matched every candidate\t%d\t%s (the filter did nothing; ranking did all the work)\n", filterNoop, pct(filterNoop, n))
	fmt.Fprintf(w, "  %d+ candidates with the identical claim text\t%d\t%s (the top-%d is a tie broken arbitrarily)\n", k, tiedTopK, pct(tiedTopK, n), k)
	fmt.Fprintf(w, "\nSTOP-WORD-AWARE BASELINE (english)\t\t\n")
	fmt.Fprintf(w, "  mean overlap vector vs english\t%s\t\n", mean(sumVE))
	fmt.Fprintf(w, "  mean overlap simple vs english\t%s\t\n", mean(sumSE))
	fmt.Fprintf(w, "\nRETURNED NOTHING (not coverage - both usually return %d)\t\t\n", k)
	fmt.Fprintf(w, "  vector\t%d\t%s\n", vectorEmpty, pct(vectorEmpty, n))
	fmt.Fprintf(w, "  lexical (simple)\t%d\t%s\n", simpleEmpty, pct(simpleEmpty, n))
	fmt.Fprintf(w, "  lexical (english)\t%d\t%s\n", engEmpty, pct(engEmpty, n))
	w.Flush()

	fmt.Println("\nNo winner is declared. Nobody has labelled which precedent was the right")
	fmt.Println("one to retrieve, so overlap is disagreement, not quality. Read the tie")
	fmt.Println("line before the overlap line: disagreement among identical texts is not")
	fmt.Println("disagreement about retrieval.")

	if !verbose {
		return
	}
	sort.Slice(results, func(i, j int) bool {
		return overlapOf(results[i].vector, results[i].simple) < overlapOf(results[j].vector, results[j].simple)
	})
	for _, c := range results {
		o := overlapOf(c.vector, c.simple)
		if o == len(c.vector) && o == len(c.simple) {
			continue
		}
		fmt.Printf("\n--- dispute %d  (candidates %d, matched %d, identical text %d)\nclaim: %s\n",
			c.target.id, c.candidates, c.matched, c.identical, truncate(c.target.claim, 90))
		fmt.Printf("  vector : %s\n", refs(c.vector))
		fmt.Printf("  simple : %s\n", refs(c.simple))
		fmt.Printf("  english: %s\n", refs(c.english))
	}
}

func refs(ps []agent.Precedent) string {
	if len(ps) == 0 {
		return "(nothing)"
	}
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, fmt.Sprintf("%s[%s %.2f]", p.Reference, p.Outcome, p.Similarity))
	}
	return strings.Join(out, "  ")
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

func pct(n, total int) string {
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("%.0f%%", 100*float64(n)/float64(total))
}
