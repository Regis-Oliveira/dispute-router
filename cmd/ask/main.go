// Command ask answers one question about the dispute queue.
//
// It is the operator-facing surface over the read-only tools: a model in a
// loop, asked something in plain words, allowed to look things up and told
// when to stop. It is what internal/agent/loop.go exists for. The drafting
// flow deliberately does not use the loop - two judges need one record - so
// without this command the loop was a harness nothing ran.
//
//	ask "what is due in the next 24 hours?"
//	ask -dry-run                 the tools and the budget, spending nothing
//
// Every real question costs money, bounded by -max-cost and -turns.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/internal/agent"
	"github.com/regisoliveira/dispute-router/internal/api"
	"github.com/regisoliveira/dispute-router/internal/config"
)

// The operator's assistant reads; it never decides. Everything it says has to
// come out of a tool result, and the tools already say when they truncated.
const system = `You answer questions about a chargeback platform's dispute queue for the people who operate it. You have read-only tools over the queue, the disputes, customer history and a summary; use them, and answer only from what they return. If a tool result says it is truncated or capped, say so rather than totalling what you saw. Quote amounts exactly as the tools format them (the "amount" field), never from the minor-unit integers. Do not recommend refunding, representing or writing anything off: that is a person's decision, and you have no tool that could act on it anyway. Be brief and concrete; ids and references are more useful to an operator than adjectives.`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dryRun  = flag.Bool("dry-run", false, "list the tools and the budget, and stop without spending")
		maxCost = flag.Float64("max-cost", 0.05, "stop once this many dollars have been spent")
		turns   = flag.Int("turns", 8, "at most this many model turns")
		asJSON  = flag.Bool("json", false, "print the full result, trace included, as JSON")
		timeout = flag.Duration("timeout", 3*time.Minute, "wall clock ceiling")
	)
	flag.Parse()
	question := strings.TrimSpace(strings.Join(flag.Args(), " "))

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

	registry := agent.NewRegistry(api.NewStore(pool))
	budget := agent.Budget{
		MaxTurns:      *turns,
		MaxCostMicros: int64(*maxCost * 1_000_000),
		MaxTokens:     1024,
	}

	if *dryRun || question == "" {
		fmt.Printf("tools: %s\n", strings.Join(registry.Names(), ", "))
		fmt.Printf("budget: %d turn(s), $%.4f, %d output tokens per turn, model %s\n",
			budget.MaxTurns, *maxCost, budget.MaxTokens, cfg.AnthropicModel)
		if question == "" && !*dryRun {
			return fmt.Errorf("nothing asked: pass the question as the argument")
		}
		return nil
	}

	completer, err := agent.NewAnthropic(cfg.AnthropicAPIKey, cfg.AnthropicModel)
	if err != nil {
		return err
	}
	pricing := agent.Pricing{
		InputMicrosPerMTok:  cfg.AgentInputPerMTok,
		OutputMicrosPerMTok: cfg.AgentOutputPerMTok,
	}

	result, err := agent.NewLoop(completer, registry, cfg.AnthropicModel, pricing, budget).
		Run(ctx, system, question)
	if err != nil {
		return err
	}

	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(result)
	}

	fmt.Println(result.Text)
	if !result.Halt.Done() {
		// Not a finished answer. Said in words, because the text above may
		// read as one.
		fmt.Printf("\n[stopped: %s - the answer above is not finished]\n", result.Halt)
	}

	calls := 0
	for _, turn := range result.Turns {
		calls += len(turn.ToolCalls)
	}
	fmt.Fprintf(os.Stderr, "\n%d turn(s), %d tool call(s), %d in / %d out tokens, $%.4f\n",
		len(result.Turns), calls, result.Usage.InputTokens, result.Usage.OutputTokens,
		float64(result.CostMicros)/1_000_000)
	for _, turn := range result.Turns {
		for _, call := range turn.ToolCalls {
			fmt.Fprintf(os.Stderr, "  %s %s -> %d bytes%s\n",
				call.Name, string(call.Input), call.ResultBytes,
				map[bool]string{true: " (error)", false: ""}[call.IsError])
		}
	}
	return nil
}
