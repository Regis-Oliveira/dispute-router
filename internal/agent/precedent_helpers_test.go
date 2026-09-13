package agent

import (
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func draftResponseFor(t *testing.T, recommendation Recommendation, letter string) Response {
	t.Helper()
	input, err := json.Marshal(map[string]any{
		"recommendation": recommendation,
		"letter":         letter,
		"cited_evidence": []string{},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return Response{
		StopReason: "tool_use",
		Content: []ContentBlock{{
			Type: "tool_use", ID: "toolu_p", Name: RepresentmentTool, Input: input,
		}},
	}
}

func verdictPass(t *testing.T) Response {
	t.Helper()
	input, err := json.Marshal(map[string]any{"pass": true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return Response{
		StopReason: "tool_use",
		Content: []ContentBlock{{
			Type: "tool_use", ID: "toolu_v", Name: VerdictTool, Input: input,
		}},
	}
}

// pgxpoolHandle exists so the live tests can name the pool without importing
// pgxpool into every file that needs one.
type pgxpoolHandle struct{ pool *pgxpool.Pool }
