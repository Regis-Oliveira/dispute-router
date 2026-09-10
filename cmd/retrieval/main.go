// Command retrieval compares the two precedent strategies on the same disputes.
//
// It exists to answer one question with a number instead of a preference: does
// the vector index retrieve better than Postgres full-text search here? For
// short claims in one language, already filtered to one merchant and to settled
// disputes, that contest is genuinely open - and a vector index nobody has
// compared against the baseline is a claim rather than a result.
//
// Embedding the query costs a little; everything else is local. It never writes.
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

type comparison struct {
	dispute  int64
	claim    string
	vector   []agent.Precedent
	lexical  []agent.Precedent
	overlap  int
	agreeTop bool
}

func run() error {
	var (
		sample  = flag.Int("n", 20, "how many disputes to compare")
		limit   = flag.Int("k", 3, "precedents per strategy")
		verbose = flag.Bool("v", false, "print each disagreement")
		timeout = flag.Duration("timeout", 10*time.Minute, "wall clock ceiling")
	)
	flag.Parse()

	cfg, err := config.Load(os.Getenv("DOTENV_PATH"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	embedder, err := agent.NewVoyage(cfg.VoyageAPIKey, cfg.VoyageModel)
	if err != nil {
		return err
	}

	withVector := agent.NewRetriever(pool, embedder, *limit)
	withText := agent.NewRetriever(pool, nil, *limit)

	// Open disputes with a claim: the population the agent actually drafts for.
	rows, err := pool.Query(ctx, `
		SELECT d.id, m.external_id, d.cardholder_claim
		  FROM disputes d JOIN merchants m ON m.id = d.merchant_id
		 WHERE d.kind = 'chargeback' AND d.cardholder_claim <> ''
		   AND d.state = 'received' AND d.deadline_at > now()
		 ORDER BY d.id
		 LIMIT $1`, *sample)
	if err != nil {
		return err
	}

	type target struct {
		id       int64
		merchant string
		claim    string
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.merchant, &t.claim); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, t)
	}
	rows.Close()

	// Every query embedded in one request, then searched locally. One request
	// per search would be one request per dispute, which a rate-limited account
	// refuses long before the comparison finishes - and the batching is correct
	// regardless of the limit.
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
		vec, err := withVector.NearestTo(ctx, t.id, t.merchant, queries[i])
		if err != nil {
			return fmt.Errorf("dispute %d vector: %w", t.id, err)
		}
		lex, err := withText.Lexical(ctx, t.id, t.merchant, t.claim)
		if err != nil {
			return fmt.Errorf("dispute %d lexical: %w", t.id, err)
		}

		c := comparison{dispute: t.id, claim: t.claim, vector: vec, lexical: lex}
		c.overlap = overlapOf(vec, lex)
		c.agreeTop = len(vec) > 0 && len(lex) > 0 && vec[0].Reference == lex[0].Reference
		results = append(results, c)
	}

	report(results, *limit, *verbose)
	return nil
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

// report says what the two strategies did differently, and deliberately does
// not declare a winner.
//
// Overlap is not quality. Neither strategy has ground truth here - nobody has
// labelled which precedent was the RIGHT one to retrieve - so what this can
// honestly show is where they disagree and what only one of them finds. Turning
// that into a score would be inventing the label the dataset does not have.
func report(results []comparison, k int, verbose bool) {
	var (
		bothEmpty, vectorOnly, lexicalOnly, agreed, identical int
		totalOverlap                                          int
	)
	for _, c := range results {
		switch {
		case len(c.vector) == 0 && len(c.lexical) == 0:
			bothEmpty++
		case len(c.lexical) == 0:
			vectorOnly++
		case len(c.vector) == 0:
			lexicalOnly++
		}
		if c.agreeTop {
			agreed++
		}
		if c.overlap == len(c.vector) && c.overlap == len(c.lexical) && c.overlap > 0 {
			identical++
		}
		totalOverlap += c.overlap
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "disputes compared\t%d\ttop-%d each\n", len(results), k)
	fmt.Fprintf(w, "identical result sets\t%d\t%s\n", identical, pct(identical, len(results)))
	fmt.Fprintf(w, "same top precedent\t%d\t%s\n", agreed, pct(agreed, len(results)))
	fmt.Fprintf(w, "mean overlap\t%.2f of %d\t\n", float64(totalOverlap)/float64(max(1, len(results))), k)
	fmt.Fprintf(w, "found only by vector\t%d\t%s\n", vectorOnly, pct(vectorOnly, len(results)))
	fmt.Fprintf(w, "found only by lexical\t%d\t%s\n", lexicalOnly, pct(lexicalOnly, len(results)))
	fmt.Fprintf(w, "neither found anything\t%d\t%s\n", bothEmpty, pct(bothEmpty, len(results)))
	w.Flush()

	fmt.Println("\nNo winner is declared. Nobody has labelled which precedent was the right")
	fmt.Println("one to retrieve, so overlap is disagreement, not quality. What it can say")
	fmt.Println("is whether the vector index is finding anything the baseline misses.")

	if !verbose {
		return
	}
	sort.Slice(results, func(i, j int) bool { return results[i].overlap < results[j].overlap })
	for _, c := range results {
		if c.overlap == len(c.vector) && c.overlap == len(c.lexical) {
			continue
		}
		fmt.Printf("\n--- dispute %d\nclaim: %s\n", c.dispute, truncate(c.claim, 90))
		fmt.Printf("  vector : %s\n", refs(c.vector))
		fmt.Printf("  lexical: %s\n", refs(c.lexical))
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
