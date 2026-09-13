package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime/trace"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/internal/agent"
)

// startTrace records the runtime's execution trace for this invocation: every
// goroutine, every network wait, every GC cycle, while the process is waiting
// on a model. `go tool trace <file>` reads it.
func startTrace(path string) (func(), error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("trace file: %w", err)
	}
	if err := trace.Start(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("start trace: %w", err)
	}
	return func() { trace.Stop(); f.Close() }, nil
}

// runRow is one agent_runs row as the report reads it: the columns it
// aggregates, plus the stored trace and findings decoded into the types that
// wrote them.
type runRow struct {
	Outcome        agent.Outcome
	Recommendation agent.Recommendation
	Attempt        int
	InputTokens    int
	OutputTokens   int
	CostMicros     int64
	Wall           time.Duration
	Findings       []agent.Finding
	Trace          agent.Trace
}

// printReport reads agent_runs and says what the assistant has done and what
// it cost, with the numbers a person comparing two days of runs needs:
// outcomes, cost and latency distributions, how much input the cache served,
// which retrieval ran, and which rules drafts broke. It spends nothing.
func printReport(ctx context.Context, pool *pgxpool.Pool, w io.Writer) error {
	rows, err := pool.Query(ctx, `
		SELECT outcome, coalesce(recommendation, ''), attempt,
		       input_tokens, output_tokens, cost_micros,
		       EXTRACT(EPOCH FROM (finished_at - started_at)) * 1e9,
		       findings::text, trace::text
		  FROM agent_runs ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read runs: %w", err)
	}
	defer rows.Close()

	var runs []runRow
	for rows.Next() {
		var r runRow
		var findings, tr string
		var wall float64
		if err := rows.Scan(&r.Outcome, &r.Recommendation, &r.Attempt, &r.InputTokens, &r.OutputTokens,
			&r.CostMicros, &wall, &findings, &tr); err != nil {
			return fmt.Errorf("scan run: %w", err)
		}
		r.Wall = time.Duration(wall)
		_ = json.Unmarshal([]byte(findings), &r.Findings)
		_ = json.Unmarshal([]byte(tr), &r.Trace)
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	if len(runs) == 0 {
		fmt.Fprintln(tw, "no runs recorded yet")
		return nil
	}

	outcomes := map[string]int{}
	retrieval := map[string]int{}
	checks := map[string]int{}
	var cost, tokensIn, tokensOut, cacheRead, cacheWrite, escalated, retries, withPrecedent int
	var withFiles, withFilesRead int
	var walls, gens, vers, assemblies []time.Duration
	for _, r := range runs {
		outcomes[string(r.Outcome)]++
		cost += int(r.CostMicros)
		tokensIn += r.InputTokens
		tokensOut += r.OutputTokens
		g := r.Trace.Generator.Usage
		cacheRead += g.CacheReadInputTokens
		cacheWrite += g.CacheCreationInputTokens
		if v := r.Trace.Verifier; v != nil {
			cacheRead += v.Usage.CacheReadInputTokens
			cacheWrite += v.Usage.CacheCreationInputTokens
			vers = append(vers, v.Latency)
		}
		gens = append(gens, r.Trace.Generator.Latency)
		walls = append(walls, r.Wall)
		assemblies = append(assemblies, r.Trace.Assembly)
		// The buckets are display labels rather than the vocabulary itself:
		// a row written before the trace carried a method has none, and
		// "unrecorded" is not a retrieval strategy.
		method := string(r.Trace.Retrieval.Method)
		if method == "" {
			method = "unrecorded"
		}
		retrieval[method]++
		if r.Trace.Precedents > 0 {
			withPrecedent++
		}
		if r.Trace.Evidence.Files > 0 {
			withFiles++
		}
		if r.Trace.Evidence.Read > 0 {
			withFilesRead++
		}
		if r.Trace.Escalated {
			escalated++
		}
		if r.Attempt > 1 {
			retries++
		}
		for _, f := range r.Findings {
			checks[string(f.Check)]++
		}
	}

	fmt.Fprintf(tw, "runs\t%d\n", len(runs))
	fmt.Fprintf(tw, "\nOUTCOMES\t\n")
	for _, k := range sortedKeys(outcomes) {
		fmt.Fprintf(tw, "  %s\t%d\t%s\n", k, outcomes[k], pct(outcomes[k], len(runs)))
	}
	fmt.Fprintf(tw, "  escalated to a person\t%d\n", escalated)
	fmt.Fprintf(tw, "  second attempts\t%d\n", retries)

	fmt.Fprintf(tw, "\nCOST\t\n")
	fmt.Fprintf(tw, "  total\t$%.4f\n", float64(cost)/1e6)
	fmt.Fprintf(tw, "  per run\t$%.4f\n", float64(cost)/1e6/float64(len(runs)))
	fmt.Fprintf(tw, "  tokens\t%d in / %d out\n", tokensIn, tokensOut)
	cacheable := tokensIn + cacheRead + cacheWrite
	if cacheable > 0 {
		fmt.Fprintf(tw, "  input served from cache\t%s\t(%d read, %d written)\n", pct(cacheRead, cacheable), cacheRead, cacheWrite)
	}

	fmt.Fprintf(tw, "\nLATENCY (wall, per run)\t\n")
	fmt.Fprintf(tw, "  assemble the record\tp50 %s\tp95 %s\n", dur(percentile(assemblies, 50)), dur(percentile(assemblies, 95)))
	fmt.Fprintf(tw, "  generator call\tp50 %s\tp95 %s\n", dur(percentile(gens, 50)), dur(percentile(gens, 95)))
	if len(vers) > 0 {
		fmt.Fprintf(tw, "  verifier call\tp50 %s\tp95 %s\n", dur(percentile(vers, 50)), dur(percentile(vers, 95)))
	}
	fmt.Fprintf(tw, "  whole run\tp50 %s\tp95 %s\n", dur(percentile(walls, 50)), dur(percentile(walls, 95)))

	fmt.Fprintf(tw, "\nRETRIEVAL\t\n")
	for _, k := range sortedKeys(retrieval) {
		fmt.Fprintf(tw, "  %s\t%d\t%s\n", k, retrieval[k], pct(retrieval[k], len(runs)))
	}
	fmt.Fprintf(tw, "  runs with at least one precedent\t%d\t%s\n", withPrecedent, pct(withPrecedent, len(runs)))

	fmt.Fprintf(tw, "\nEVIDENCE\t\n")
	fmt.Fprintf(tw, "  runs with a file on the dispute\t%d\t%s\n", withFiles, pct(withFiles, len(runs)))
	fmt.Fprintf(tw, "  runs with a file's text in the record\t%d\t%s\n", withFilesRead, pct(withFilesRead, len(runs)))

	if len(checks) > 0 {
		fmt.Fprintf(tw, "\nFINDINGS BY RULE\t\n")
		for _, k := range sortedKeys(checks) {
			fmt.Fprintf(tw, "  %s\t%d\n", k, checks[k])
		}
	}
	return nil
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] || (m[keys[i]] == m[keys[j]] && keys[i] < keys[j]) })
	return keys
}

func percentile(values []time.Duration, p int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (len(sorted) - 1) * p / 100
	return sorted[idx]
}

func dur(d time.Duration) string { return d.Round(time.Millisecond).String() }

func pct(n, total int) string {
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("%.0f%%", 100*float64(n)/float64(total))
}
