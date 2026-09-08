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

func liveStore(t *testing.T) *api.Store {
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
	return api.NewStore(pool)
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
	store := liveStore(t)
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
	store := liveStore(t)
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
