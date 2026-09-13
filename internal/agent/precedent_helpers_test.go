package agent

import (
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/internal/llm"
)

func draftResponseFor(t *testing.T, recommendation Recommendation, letter string) llm.Response {
	t.Helper()
	input, err := json.Marshal(map[string]any{
		"recommendation": recommendation,
		"letter":         letter,
		"cited_evidence": []string{},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return llm.Response{
		StopReason: "tool_use",
		Content: []llm.ContentBlock{{
			Type: "tool_use", ID: "toolu_p", Name: RepresentmentTool, Input: input,
		}},
	}
}

func verdictPass(t *testing.T) llm.Response {
	t.Helper()
	input, err := json.Marshal(map[string]any{"pass": true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return llm.Response{
		StopReason: "tool_use",
		Content: []llm.ContentBlock{{
			Type: "tool_use", ID: "toolu_v", Name: VerdictTool, Input: input,
		}},
	}
}

// pgxpoolHandle exists so the live tests can name the pool without importing
// pgxpool into every file that needs one.
type pgxpoolHandle struct{ pool *pgxpool.Pool }
