package toolloop

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/internal/api"
	"github.com/regisoliveira/dispute-router/internal/disputetools"
	"github.com/regisoliveira/dispute-router/internal/llm"
)

// Most of what matters at this boundary is decided before a query runs, so
// most of these tests never open a connection. A registry over a store with no
// pool is enough to exercise every path that rejects, and the paths that reject
// are the ones worth pinning.
func offlineRegistry() *Registry {
	return NewRegistry(api.NewStore(nil))
}

// The request the model sees is built entirely from Go struct tags. If that
// derivation breaks, the tools still exist and are simply undescribed - which
// fails silently and looks like the model getting worse at its job.
func TestEveryToolIsFullyDescribed(t *testing.T) {
	for _, tool := range offlineRegistry().Tools() {
		if tool.Name == "" {
			t.Fatal("a tool has no name")
		}
		if len(tool.Description) < 40 {
			t.Errorf("tool %q has a %d character description; the description is the prompt",
				tool.Name, len(tool.Description))
		}

		var schema struct {
			Type       string `json:"type"`
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("tool %q has an unparseable input_schema: %v", tool.Name, err)
		}
		if schema.Type != "object" {
			t.Errorf("tool %q input_schema is type %q; the API requires object", tool.Name, schema.Type)
		}
		for field, property := range schema.Properties {
			if property.Description == "" {
				t.Errorf("tool %q field %q has no description; the model has nothing to go on",
					tool.Name, field)
			}
		}
	}
}

// Optional and required are carried by omitempty on the struct field and
// nothing else, so it is worth checking the two ends of that: an id the tool
// cannot work without, and a set of filters that are all optional.
func TestRequiredFieldsComeFromTheStruct(t *testing.T) {
	schemas := map[string]json.RawMessage{}
	for _, tool := range offlineRegistry().Tools() {
		schemas[tool.Name] = tool.InputSchema
	}

	required := func(name string) []string {
		var schema struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(schemas[name], &schema); err != nil {
			t.Fatalf("unmarshal %s: %v", name, err)
		}
		return schema.Required
	}

	if got := required("get_dispute"); len(got) != 1 || got[0] != "id" {
		t.Errorf("get_dispute requires %v, want [id]", got)
	}
	if got := required("list_disputes"); len(got) != 0 {
		t.Errorf("list_disputes requires %v; every filter on it is optional", got)
	}
}

// A guessed tool name is a message, not a crash - and the message has to carry
// the real names, or the model has no way to correct itself.
func TestUnknownToolIsAnswered(t *testing.T) {
	result, err := offlineRegistry().Run(context.Background(), llm.ToolUse{
		ID: "toolu_1", Name: "refund_dispute", Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("an unknown tool name aborted the run: %v", err)
	}
	if !result.IsError {
		t.Error("unknown tool did not come back as an error result")
	}
	if !strings.Contains(result.Content, "list_disputes") {
		t.Errorf("refusal does not name the available tools: %q", result.Content)
	}
	if result.ToolUseID != "toolu_1" {
		t.Errorf("tool_use_id = %q, want toolu_1; the API cannot match the result otherwise",
			result.ToolUseID)
	}
}

// The case this whole classification exists for. A model that filters on
// "merchant_id" when the field is "merchant" must be told, because the
// alternative - accepting the call and ignoring the field - answers with every
// merchant's disputes while the model believes its filter applied.
func TestUnknownArgumentIsRefusedNotIgnored(t *testing.T) {
	result, err := offlineRegistry().Run(context.Background(), llm.ToolUse{
		ID: "toolu_2", Name: "list_disputes", Input: json.RawMessage(`{"merchant_id":"mrc_northwind"}`),
	})
	if err != nil {
		t.Fatalf("a bad argument aborted the run: %v", err)
	}
	if !result.IsError {
		t.Fatal("an unknown argument was accepted; the filter would have been silently dropped")
	}
	if !strings.Contains(result.Content, "merchant_id") {
		t.Errorf("refusal does not say which field was wrong: %q", result.Content)
	}
}

// Same rule for a value of the wrong type: answerable, so it is answered.
func TestWrongArgumentTypeIsAnswered(t *testing.T) {
	result, err := offlineRegistry().Run(context.Background(), llm.ToolUse{
		ID: "toolu_3", Name: "list_disputes", Input: json.RawMessage(`{"limit":"fifty"}`),
	})
	if err != nil {
		t.Fatalf("a wrongly typed argument aborted the run: %v", err)
	}
	if !result.IsError {
		t.Error("a string where an integer belongs was accepted")
	}
}

// The tool surface is recorded per run in the audit trail, so it has to match
// what the model was actually handed.
func TestNamesMatchTheAdvertisedTools(t *testing.T) {
	r := offlineRegistry()
	names, tools := r.Names(), r.Tools()
	if len(names) != len(tools) {
		t.Fatalf("Names() has %d entries, Tools() has %d", len(names), len(tools))
	}
	for i := range tools {
		if names[i] != tools[i].Name {
			t.Errorf("Names()[%d] = %q, Tools()[%d].Name = %q", i, names[i], i, tools[i].Name)
		}
	}
}

// The byte ceiling is meant to be headroom, not a wall: a full page of the
// largest list has to fit, or the model spends a turn discovering it cannot ask
// for one. This is the only assertion here that needs real data.
func TestAFullPageFitsUnderTheCeiling(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping the sizing check")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	result, err := NewRegistry(api.NewStore(pool)).Run(ctx, llm.ToolUse{
		ID: "toolu_4", Name: "list_disputes", Input: json.RawMessage(`{"limit":50}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.IsError {
		t.Fatalf("a full page was refused: %s", result.Content)
	}
	if len(result.Content) > maxResultBytes {
		t.Errorf("full page is %d bytes against a %d byte ceiling", len(result.Content), maxResultBytes)
	}
	t.Logf("a full 50-row page is %d bytes, %.0f%% of the ceiling",
		len(result.Content), 100*float64(len(result.Content))/float64(maxResultBytes))
}

// A refusal the model reads is prose; a refusal an operator counts is a rule.
// Every way the registry can say no names the rule it said no under.
func TestEveryRefusalNamesItsRule(t *testing.T) {
	registry := offlineRegistry()
	ctx := context.Background()

	unknown, err := registry.Run(ctx, llm.ToolUse{ID: "t1", Name: "drop_table", Input: json.RawMessage(`{}`)})
	if err != nil || unknown.Rule != RuleUnknownTool {
		t.Errorf("unknown tool: rule %q, err %v; want %q", unknown.Rule, err, RuleUnknownTool)
	}

	badArg, err := registry.Run(ctx, llm.ToolUse{ID: "t2", Name: "list_disputes", Input: json.RawMessage(`{"merchant_id":"x"}`)})
	if err != nil || badArg.Rule != RuleInvalidArguments {
		t.Errorf("unknown argument: rule %q, err %v; want %q", badArg.Rule, err, RuleInvalidArguments)
	}

	// A result over the ceiling, from a tool that exists only in this test.
	huge := &Registry{byName: map[string]disputetools.Definition{
		"huge": {Name: "huge", Invoke: func(context.Context, json.RawMessage) (any, error) {
			return strings.Repeat("x", maxResultBytes+1), nil
		}},
	}}
	big, err := huge.Run(ctx, llm.ToolUse{ID: "t3", Name: "huge", Input: json.RawMessage(`{}`)})
	if err != nil || big.Rule != RuleResultTooLarge || !big.IsError {
		t.Errorf("oversized result: rule %q, is_error %v, err %v; want %q", big.Rule, big.IsError, err, RuleResultTooLarge)
	}
}
