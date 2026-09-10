package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/internal/api"
	"github.com/regisoliveira/dispute-router/internal/disputetools"
)

// A lister that hands back a file complete with the presigned URL the real one
// produces, so the test can check what survives the crossing.
type fakeEvidence struct{ files []api.EvidenceFile }

func (f fakeEvidence) List(context.Context, int64) ([]api.EvidenceFile, error) {
	return f.files, nil
}

// Returns the pool alongside the store: a couple of these tests need to pick a
// row by a condition the store has no method for, and widening the store's API
// for a test would be the wrong trade.
func liveStore(t *testing.T) (*api.Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping fact assembly tests")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return api.NewStore(pool), pool
}

func someDisputeID(t *testing.T, store *api.Store) int64 {
	t.Helper()
	list, err := disputetools.New(store).ListDisputes(context.Background(),
		disputetools.ListDisputesInput{Limit: 1})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Disputes) == 0 {
		t.Skip("no disputes seeded")
	}
	return list.Disputes[0].ID
}

// A presigned URL is a bearer credential with a TTL. It must not survive into
// a prompt, because a prompt becomes a transcript that gets logged, replayed
// and pasted into tickets.
func TestFactsCarryEvidenceNamesAndNotTheirURLs(t *testing.T) {
	store, _ := liveStore(t)
	const secret = "https://s3.example/evidence.pdf?X-Amz-Signature=deadbeef"

	facts, err := NewFactSource(store, fakeEvidence{files: []api.EvidenceFile{{
		Key:        "dispute/1/receipt.pdf",
		Name:       "receipt.pdf",
		SizeBytes:  4096,
		UploadedAt: time.Now(),
		URL:        secret,
	}}}).For(context.Background(), someDisputeID(t, store))
	if err != nil {
		t.Fatalf("For: %v", err)
	}

	if len(facts.Evidence) != 1 || facts.Evidence[0].Name != "receipt.pdf" {
		t.Fatalf("evidence = %+v", facts.Evidence)
	}

	// Checked on the encoded form, because that is what actually reaches a
	// model - a field that is dropped from the struct but re-added later would
	// slip past an assertion on the struct alone.
	encoded, err := json.Marshal(facts)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "X-Amz-Signature") || strings.Contains(string(encoded), secret) {
		t.Error("the presigned URL reached the facts block")
	}
	if !strings.Contains(string(encoded), "receipt.pdf") {
		t.Error("the filename did not; a draft has to be able to cite it by name")
	}
}

// The facts are read from the store, not taken from a transcript, and the
// masking the tools apply comes with them.
func TestFactsAreReadFromTheStoreAndStayMasked(t *testing.T) {
	store, _ := liveStore(t)
	id := someDisputeID(t, store)

	facts, err := NewFactSource(store, fakeEvidence{}).For(context.Background(), id)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if facts.Dispute.ID != id {
		t.Errorf("assembled facts for dispute %d, asked for %d", facts.Dispute.ID, id)
	}
	if facts.Dispute.CustomerEmail != "" && !strings.Contains(facts.Dispute.CustomerEmail, "***") {
		t.Errorf("customer email %q reached the facts block unmasked", facts.Dispute.CustomerEmail)
	}
	// The history is the signal the whole draft turns on, and it only arrives
	// if the merchant handle chained correctly.
	if len(facts.History) == 0 {
		t.Error("no customer history; a dispute always appears in its own")
	}
}

// A dispute with a real cardholder claim, so the boundary can be tested against
// what the seed actually produced rather than against a fixture.
func disputeWithAClaim(t *testing.T, pool *pgxpool.Pool, like string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`SELECT id FROM disputes WHERE cardholder_claim LIKE $1 ORDER BY id LIMIT 1`, like).Scan(&id)
	if err != nil {
		t.Skipf("no seeded dispute matching %q: %v", like, err)
	}
	return id
}

// The claim reaches the agent, because the agent quarantines it.
func TestTheClaimReachesTheFacts(t *testing.T) {
	store, pool := liveStore(t)
	id := disputeWithAClaim(t, pool, "The order never arrived%")

	facts, err := NewFactSource(store, fakeEvidence{}).For(context.Background(), id)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if facts.CardholderClaim == "" {
		t.Fatal("the cardholder claim did not reach the facts block")
	}

	rendered, err := facts.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	at := strings.Index(rendered, facts.CardholderClaim)
	openAt, closeAt := strings.Index(rendered, claimOpen), strings.Index(rendered, claimClose)
	if at < openAt || at > closeAt {
		t.Error("a real seeded claim rendered outside the quarantine")
	}
}

// It does not reach the MCP tool output, which has nowhere to mark it untrusted.
func TestTheClaimNeverReachesTheToolOutput(t *testing.T) {
	store, pool := liveStore(t)
	id := disputeWithAClaim(t, pool, "%Ignore all previous instructions%")

	out, err := disputetools.New(store).GetDispute(context.Background(),
		disputetools.GetDisputeInput{ID: id})
	if err != nil {
		t.Fatalf("GetDispute: %v", err)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "Ignore all previous instructions") {
		t.Error("hostile cardholder text reached the MCP tool output unmarked")
	}

	// And the same dispute, through the agent's path, does carry it.
	facts, err := NewFactSource(store, fakeEvidence{}).For(context.Background(), id)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if !strings.Contains(facts.CardholderClaim, "Ignore all previous instructions") {
		t.Error("the agent path lost the claim it is supposed to quarantine")
	}
}
