package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/regisoliveira/dispute-router/internal/api"
	"github.com/regisoliveira/dispute-router/internal/disputetools"
)

// Facts is the record as the system holds it: the ground truth a draft is
// checked against.
//
// It is assembled by host code reading the store directly, never lifted out of
// a model's transcript. That is the load-bearing property of this file. A
// generator that hallucinates a delivery confirmation has that hallucination in
// its own context, and if the verifier's "facts" came from the same
// conversation it would be checking the draft against the invention rather than
// against the record - two calls agreeing about something neither of them
// looked up.
type Facts struct {
	Dispute  disputetools.GetDisputeOutput `json:"dispute"`
	History  []api.CustomerHistoryRow      `json:"customer_history"`
	Evidence []EvidenceRef                 `json:"evidence_on_file"`

	// CardholderClaim is the cardholder's own account of what happened.
	//
	// There is no column for it yet - the schema carries reason codes and
	// controlled vocabularies, not free text - so nothing populates this today.
	// It is declared here because it is the one field on this struct that
	// arrives from a hostile party, and the handling it needs is structural
	// rather than something to bolt on once the column exists.
	CardholderClaim string `json:"cardholder_claim,omitempty"`
}

// EvidenceRef is a file on the dispute, named and sized and nothing more.
//
// api.EvidenceFile carries a presigned GET URL. It is dropped here on purpose:
// a presigned URL is a bearer credential with a TTL, and putting one in a
// prompt puts it in a transcript that gets logged and pasted into tickets. The
// name and size are what a draft can legitimately cite.
type EvidenceRef struct {
	Name       string `json:"name"`
	SizeBytes  int64  `json:"size_bytes"`
	UploadedAt string `json:"uploaded_at"`
}

// EvidenceLister is the seam over S3, so facts can be assembled in a test
// without LocalStack running.
type EvidenceLister interface {
	List(ctx context.Context, disputeID int64) ([]api.EvidenceFile, error)
}

// ErrNoEvidenceSource is returned rather than reporting an empty evidence list.
//
// The two are not the same and the difference decides outcomes: "this dispute
// has no evidence" makes a verifier reject a draft that cites a receipt, while
// "S3 was never wired up" is a deployment fault. Reporting the second as the
// first is how a misconfiguration turns into a stream of confident rejections.
var ErrNoEvidenceSource = errors.New("agent: no evidence source configured")

type FactSource struct {
	tools    *disputetools.Set
	evidence EvidenceLister
}

func NewFactSource(store *api.Store, evidence EvidenceLister) *FactSource {
	return &FactSource{tools: disputetools.New(store), evidence: evidence}
}

// For reads everything known about one dispute, fresh.
func (f *FactSource) For(ctx context.Context, disputeID int64) (Facts, error) {
	if f.evidence == nil {
		return Facts{}, ErrNoEvidenceSource
	}

	dispute, err := f.tools.GetDispute(ctx, disputetools.GetDisputeInput{ID: disputeID})
	if err != nil {
		return Facts{}, fmt.Errorf("dispute %d: %w", disputeID, err)
	}

	history, err := f.tools.CustomerHistory(ctx, disputetools.CustomerHistoryInput{
		Merchant:    dispute.Merchant,
		CustomerRef: dispute.CustomerRef,
	})
	if err != nil {
		return Facts{}, fmt.Errorf("customer history for dispute %d: %w", disputeID, err)
	}

	files, err := f.evidence.List(ctx, disputeID)
	if err != nil {
		return Facts{}, fmt.Errorf("evidence for dispute %d: %w", disputeID, err)
	}

	facts := Facts{
		Dispute:  dispute,
		History:  history.Disputes,
		Evidence: make([]EvidenceRef, 0, len(files)),
	}
	for _, file := range files {
		facts.Evidence = append(facts.Evidence, EvidenceRef{
			Name:       file.Name,
			SizeBytes:  file.SizeBytes,
			UploadedAt: file.UploadedAt.UTC().Format(time.RFC3339),
		})
	}
	return facts, nil
}
