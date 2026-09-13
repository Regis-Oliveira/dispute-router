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
	"io"
	"os"
	"os/signal"
	"sort"
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
		logPath = flag.String("log", "", "append one JSON line per question to this file (for example .traces/ask.jsonl)")
		timeout = flag.Duration("timeout", 3*time.Minute, "wall clock ceiling")
	)
	flag.Parse()
	question := strings.TrimSpace(strings.Join(flag.Args(), " "))

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireDatabase(); err != nil {
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
		MaxTokens:     4096, // thinking counts against this; 1,024 cut the first real answer off mid-sentence
	}

	if *dryRun || question == "" {
		fmt.Printf("tools: %s\n", strings.Join(registry.Names(), ", "))
		fmt.Printf("budget: %d turn(s), $%.4f, %d output tokens per turn, model %s\n",
			budget.MaxTurns, *maxCost, budget.MaxTokens, cfg.Agent.AnthropicModel)
		if question == "" && !*dryRun {
			return fmt.Errorf("nothing asked: pass the question as the argument")
		}
		return nil
	}

	completer, err := agent.NewAnthropic(cfg.Agent.AnthropicAPIKey, cfg.Agent.AnthropicModel)
	if err != nil {
		return err
	}
	pricing := agent.Pricing{
		InputMicrosPerMTok:  cfg.Agent.InputPerMTok,
		OutputMicrosPerMTok: cfg.Agent.OutputPerMTok,
	}

	result, err := agent.NewLoop(completer, registry, cfg.Agent.AnthropicModel, pricing, budget).
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

	session := summarise(question, cfg.Agent.AnthropicModel, result)
	session.print(os.Stderr)
	if *logPath != "" {
		if err := session.appendTo(*logPath); err != nil {
			return err
		}
	}
	return nil
}

// A session report: what one question cost and what the loop did to answer
// it. The same shape cmd/agent -report gives a drafting run, for the same
// reason - the trace already holds every fact below, and a fact nobody
// summarises is a fact nobody checks.
type session struct {
	At          time.Time        `json:"at"`
	Question    string           `json:"question"`
	Model       string           `json:"model"`
	Halt        agent.Halt       `json:"halt"`
	Turns       int              `json:"turns"`
	ToolCalls   int              `json:"tool_calls"`
	CallsByTool map[string]int   `json:"calls_by_tool"`
	Refusals    map[string]int   `json:"refusals_by_rule"`
	Usage       agent.Usage      `json:"usage"`
	CostMicros  int64            `json:"cost_micros"`
	Tools       []agent.ToolCall `json:"tools"`
}

func summarise(question, model string, result agent.Result) session {
	s := session{
		At: time.Now().UTC(), Question: question, Model: model, Halt: result.Halt,
		Turns: len(result.Turns), CallsByTool: map[string]int{}, Refusals: map[string]int{},
		Usage: result.Usage, CostMicros: result.CostMicros,
	}
	for _, turn := range result.Turns {
		for _, call := range turn.ToolCalls {
			s.ToolCalls++
			s.CallsByTool[call.Name]++
			if call.IsError {
				rule := call.Rule
				if rule == "" {
					rule = "unattributed"
				}
				s.Refusals[rule]++
			}
			s.Tools = append(s.Tools, call)
		}
	}
	return s
}

func (s session) print(w io.Writer) {
	fmt.Fprintf(w, "\n== SESSION ==\n")
	fmt.Fprintf(w, "  stopped:   %s\n", s.Halt)
	fmt.Fprintf(w, "  turns:     %d\n", s.Turns)
	fmt.Fprintf(w, "  tools:     %d call(s)", s.ToolCalls)
	for _, name := range sortedKeys(s.CallsByTool) {
		fmt.Fprintf(w, "  %s x%d", name, s.CallsByTool[name])
	}
	fmt.Fprintln(w)
	if len(s.Refusals) == 0 {
		fmt.Fprintf(w, "  refusals:  none\n")
	} else {
		fmt.Fprintf(w, "  refusals: ")
		for _, rule := range sortedKeys(s.Refusals) {
			fmt.Fprintf(w, "  %s x%d", rule, s.Refusals[rule])
		}
		fmt.Fprintln(w)
	}
	cacheable := s.Usage.InputTokens + s.Usage.CacheReadInputTokens + s.Usage.CacheCreationInputTokens
	fmt.Fprintf(w, "  tokens:    %d in / %d out", s.Usage.InputTokens, s.Usage.OutputTokens)
	if cacheable > 0 {
		fmt.Fprintf(w, "  (%.0f%% of input from cache)", 100*float64(s.Usage.CacheReadInputTokens)/float64(cacheable))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  cost:      $%.4f\n", float64(s.CostMicros)/1_000_000)
	for _, call := range s.Tools {
		mark := ""
		if call.IsError {
			mark = "  refused: " + call.Rule
		}
		fmt.Fprintf(w, "    %s %s -> %d bytes%s\n", call.Name, string(call.Input), call.ResultBytes, mark)
	}
}

// appendTo writes the session as one JSON line. A file of these answers
// "what did operators ask this week, and what did it cost" with jq, without a
// table: questions do not reach a card network, so they do not need the
// append-only trigger letters get.
func (s session) appendTo(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("session log: %w", err)
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(s)
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
