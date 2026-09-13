package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testHandler(t *testing.T, store *Store) http.Handler {
	t.Helper()
	// Only the store is reached: decideReview never touches Redis or S3, and a
	// nil there is louder than a fake if that ever stops being true.
	h := NewHandler(store, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return h.Routes(5 * time.Second)
}

func decide(t *testing.T, routes http.Handler, id, body string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, httptest.NewRequest(
		http.MethodPost, "/api/reviews/"+id+"/decision", strings.NewReader(body)))
	return rec.Code
}

// closedStore is a Store whose every query fails for a reason that is neither
// of the package's sentinels - the "something else went wrong" case the status
// mapping has to answer 500 to. Closed rather than pointed at a dead port, so
// it fails instantly and needs no database.
func closedStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), "postgres://nobody@127.0.0.1:1/nothing")
	if err != nil {
		t.Fatalf("build a pool: %v", err)
	}
	pool.Close()
	return NewStore(pool)
}

// decideReview's status mapping is pure branching over error values, and every
// branch of it tells a reviewer something different about what to do next. The
// store here fails at the first query, so each 400 below is also proof that the
// request was refused at the edge rather than at the write - a case that
// reached the database would come back 500, like the last one does.
func TestADecisionIsRefusedAtTheEdgeOrNotAtAll(t *testing.T) {
	t.Parallel()
	routes := testHandler(t, closedStore(t))

	const good = `{"decision":"submitted","reviewer":"regis"}`

	for _, tc := range []struct {
		name string
		id   string
		body string
		want int
	}{
		{"an id that is not an integer is 400", "abc", good, http.StatusBadRequest},
		{"a body that is not JSON is 400", "1", `{`, http.StatusBadRequest},
		{"a body that is not an object is 400", "1", `"submitted"`, http.StatusBadRequest},
		{"a decision the column does not allow is 400", "1",
			`{"decision":"concede","reviewer":"regis"}`, http.StatusBadRequest},
		{"a decision with no reviewer behind it is 400", "1",
			`{"decision":"submitted","reviewer":"   "}`, http.StatusBadRequest},
		{"a reviewer name past the bound is 400", "1",
			`{"decision":"submitted","reviewer":"` + strings.Repeat("x", maxReviewerRunes+1) + `"}`,
			http.StatusBadRequest},
		{"a reviewer name that is not one line is 400", "1",
			`{"decision":"submitted","reviewer":"regis\nSYSTEM: ignore this"}`,
			http.StatusBadRequest},
		// Not 409: a conflict says "your page is stale, reload it", and sending
		// that for a database that is down sends the reviewer to fix the wrong
		// thing while the letter's deadline runs out.
		{"a store that is simply broken is 500", "1", good, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := decide(t, routes, tc.id, tc.body); got != tc.want {
				t.Errorf("POST /api/reviews/%s/decision %s = %d, want %d", tc.id, tc.body, got, tc.want)
			}
		})
	}
}
