package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/regisoliveira/dispute-router/internal/api"
)

// Runs a real client against a real server over the SDK's in-memory transport.
// No subprocess, no pipes, no JSON-RPC by hand - but the same code path a
// client exercises, including schema validation and error shape.
func connect(t *testing.T) *mcp.ClientSession {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping MCP protocol tests")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(pool.Close)

	server := mcp.NewServer(&mcp.Implementation{Name: "dispute-router", Version: "test"}, nil)
	New(api.NewStore(pool)).Register(server)

	clientTransport, serverTransport := mcp.NewInMemoryTransports()

	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return session
}

// The claim the whole design rests on. If a write tool ever appears here, this
// fails - which is the point: it is easier to add a convenient tool than to
// remember why you did not.
func TestEveryToolIsReadOnly(t *testing.T) {
	session := connect(t)

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("no tools registered")
	}

	for _, tool := range tools.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %q is not marked read-only", tool.Name)
		}
		if tool.Description == "" {
			t.Errorf("tool %q has no description; the description is the prompt", tool.Name)
		}
	}
}

func TestListDisputesRespectsTheCap(t *testing.T) {
	session := connect(t)

	// Ask for far more than the cap allows.
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_disputes",
		Arguments: map[string]any{"limit": 5000},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned an error: %v", result.Content)
	}

	var out ListDisputesOutput
	decode(t, result, &out)

	if len(out.Disputes) > maxRows {
		t.Errorf("returned %d disputes, cap is %d", len(out.Disputes), maxRows)
	}
	// A truncated answer that does not say so is how an assistant states a
	// wrong total with confidence.
	if out.Total > int64(len(out.Disputes)) && out.Note == "" {
		t.Error("results were truncated with no note saying so")
	}
}

// An unknown id is a tool error, not a protocol error, and not an empty result
// that reads like "there is no such dispute, and that is fine".
func TestUnknownDisputeIsAToolError(t *testing.T) {
	session := connect(t)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_dispute",
		Arguments: map[string]any{"id": 99999999},
	})
	if err != nil {
		t.Fatalf("CallTool returned a protocol error: %v", err)
	}
	if !result.IsError {
		t.Error("a missing dispute did not set IsError")
	}
}

// Schema validation comes from the Go types, so a wrong argument type is
// rejected before the handler runs.
func TestBadArgumentTypeIsRejected(t *testing.T) {
	session := connect(t)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_disputes",
		Arguments: map[string]any{"limit": "muitos"},
	})
	if err == nil && !result.IsError {
		t.Error("a string where an integer belongs was accepted")
	}
}

// The email never leaves in the clear.
func TestDisputeDetailMasksTheEmail(t *testing.T) {
	session := connect(t)
	ctx := context.Background()

	list, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_disputes",
		Arguments: map[string]any{"limit": 1},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var listed ListDisputesOutput
	decode(t, list, &listed)
	if len(listed.Disputes) == 0 {
		t.Skip("no disputes seeded; run make seed")
	}

	detail, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_dispute",
		Arguments: map[string]any{"id": listed.Disputes[0].ID},
	})
	if err != nil {
		t.Fatalf("get_dispute: %v", err)
	}
	var got GetDisputeOutput
	decode(t, detail, &got)

	if got.CustomerEmail == "" {
		t.Fatal("no masked email returned")
	}
	if !containsRune(got.CustomerEmail, '*') {
		t.Errorf("customer_email_masked = %q, which is not masked", got.CustomerEmail)
	}
}

// An overdue dispute has to say so, not leave it to the sign of a number.
func TestOverdueIsStatedNotImplied(t *testing.T) {
	session := connect(t)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_disputes",
		Arguments: map[string]any{"due_within": "24h", "limit": 20},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var out ListDisputesOutput
	decode(t, result, &out)

	for _, d := range out.Disputes {
		if d.HoursToDeadline < 0 && !d.Overdue {
			t.Errorf("dispute %d is %dh past its deadline but overdue is false", d.ID, d.HoursToDeadline)
		}
	}
}

func decode(t *testing.T, result *mcp.CallToolResult, into any) {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

var _ = time.Second
