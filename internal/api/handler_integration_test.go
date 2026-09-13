//go:build integration

package api

import (
	"net/http"
	"strconv"
	"testing"
)

// The other half of decideReview's status mapping, which needs a run to decide
// on - so it is here rather than beside the first half in handler_test.go: the
// run comes from reviews_test.go's scratch database, and that file's integration
// tag has to come with it. The half that needs no database stays untagged.
//
// Every way of being too late is one answer, 409, and deliberately not 404:
// the reviewer is holding a page that is out of date, and telling them the run
// is missing sends them looking for the wrong problem. That includes the run
// that never existed - Store.Decide cannot tell "no such run" from "already
// decided", both being the empty result of the same guarded UPDATE, and the
// handler does not pretend otherwise.
func TestBeingTooLateToDecideIsAConflictNeverANotFound(t *testing.T) {
	store, pool := scratchStore(t)
	routes := testHandler(t, store)

	const good = `{"decision":"submitted","reviewer":"regis"}`

	for _, tc := range []struct {
		name string
		id   func(t *testing.T) string
		want int
	}{
		{
			name: "a run awaiting review is 204",
			id: func(t *testing.T) string {
				runID, _ := awaitingReview(t, pool, "drafted", `[]`)
				return strconv.FormatInt(runID, 10)
			},
			want: http.StatusNoContent,
		},
		{
			name: "a run somebody already decided is 409",
			id: func(t *testing.T) string {
				runID, _ := awaitingReview(t, pool, "drafted", `[]`)
				decided(t, store, runID, DecisionDiscarded, "first")
				return strconv.FormatInt(runID, 10)
			},
			want: http.StatusConflict,
		},
		{
			name: "a draft whose deadline has passed is 409",
			id: func(t *testing.T) string {
				runID, disputeID := awaitingReview(t, pool, "drafted", `[]`)
				if _, err := pool.Exec(t.Context(),
					"UPDATE disputes SET deadline_at = now() - interval '1 hour' WHERE id = $1",
					disputeID); err != nil {
					t.Fatalf("expire the fixture: %v", err)
				}
				return strconv.FormatInt(runID, 10)
			},
			want: http.StatusConflict,
		},
		{
			name: "a run that never existed is 409, not 404",
			id:   func(*testing.T) string { return "999999999" },
			want: http.StatusConflict,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decide(t, routes, tc.id(t), good); got != tc.want {
				t.Errorf("decision = %d, want %d", got, tc.want)
			}
		})
	}
}
