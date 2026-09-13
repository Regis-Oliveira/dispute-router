package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"

	"github.com/regisoliveira/dispute-router/internal/api"
)

// EvidenceSource is the seam over S3: what is on a dispute, and the bytes of
// one file on it. Facts can be assembled in a test without LocalStack running.
type EvidenceSource interface {
	List(ctx context.Context, disputeID int64) ([]api.EvidenceFile, error)
	Open(ctx context.Context, disputeID int64, key string) (api.EvidenceObject, error)
}

// The three answers to "what is in this file", as the model reads them.
//
// Before the contents were read at all, the record listed a filename and a
// size, and the first dispute with a file behind it showed what that buys: the
// generator cited delivery-confirmation.pdf as "supporting that the order was
// delivered", the verifier rejected the sentence because the record held no
// such thing, and both were right. A name is not a document. The status is on
// every file so that a file whose text could not be read is never mistaken for
// one that was read and said nothing.
const (
	EvidenceRead    = "read"     // the text below is the whole file
	EvidenceCut     = "cut"      // the text below is the start of the file
	EvidenceNotRead = "not read" // there is no text below, and Note says why
)

// maxEvidenceFileRunes bounds one file at the prompt boundary and
// maxEvidenceRunes bounds all of them together, on the same reasoning as the
// cardholder's claim: the prompt is paid for by the token and checked against
// a ceiling only after the call returns, so an input with no cap is an input
// whose price nobody knows before paying it. Six thousand characters is a
// dense page and a half - a receipt, a tracking history, a terms page - and
// three files of it is more than any representment cites.
//
// Files are read in the order they were uploaded, and once the block is full
// the rest are listed by name with that as the reason, so which files were
// read is decided by the record and not by which S3 call finished first.
const (
	maxEvidenceFileRunes = 6000
	maxEvidenceRunes     = 18000

	// maxEvidencePages bounds the work on a PDF, which is otherwise a 25 MB
	// file every page of which has to be interpreted before the first
	// character is known.
	maxEvidencePages = 20
)

// maxEvidenceBytes is the most that is pulled from S3 for one file. The upload
// policy already stops a file above this landing in the bucket; it is
// repeated here because this code should not depend on that one having run.
const maxEvidenceBytes = api.MaxUploadBytes

// readEvidence fills in the text and status of every file on the dispute.
//
// A file that cannot be fetched is an error, on the same reasoning as the
// list: the record failed to assemble, the run is not recorded, and the
// dispute is tried again once S3 is back. A file that was fetched and could
// not be read - an image, a scan with no text layer, a PDF that does not
// parse - is not an error. It is a fact about the file, it goes on the record
// as one, and the draft is written knowing it.
func readEvidence(ctx context.Context, source EvidenceSource, disputeID int64, files []api.EvidenceFile) ([]EvidenceRef, error) {
	refs := make([]EvidenceRef, 0, len(files))
	budget := maxEvidenceRunes
	for _, file := range files {
		ref := EvidenceRef{
			Name:       file.Name,
			SizeBytes:  file.SizeBytes,
			UploadedAt: file.UploadedAt.UTC().Format(time.RFC3339),
		}
		if budget <= 0 {
			ref.Status = EvidenceNotRead
			ref.Note = "the evidence block is full; files are read in upload order"
			refs = append(refs, ref)
			continue
		}

		object, err := source.Open(ctx, disputeID, file.Key)
		if err != nil {
			return nil, err
		}
		ref.ContentType = object.ContentType
		text, err := func() (string, error) {
			defer object.Body.Close()
			return extractText(object)
		}()
		if err != nil {
			ref.Status = EvidenceNotRead
			ref.Note = err.Error()
			refs = append(refs, ref)
			continue
		}

		ref.Status = EvidenceRead
		limit := min(budget, maxEvidenceFileRunes)
		if runes := []rune(text); len(runes) > limit {
			text = string(runes[:limit])
			ref.Status = EvidenceCut
			ref.Note = fmt.Sprintf("cut at %d characters", limit)
		}
		ref.Text = text
		budget -= utf8.RuneCountInString(text)
		refs = append(refs, ref)
	}
	return refs, nil
}

// extractText turns a file into the text a model can be shown, or says why it
// cannot.
//
// The decision is made on the content type S3 stored, which is the one the
// upload policy pinned, not on the filename: the name is typed by whoever
// uploaded and the type was signed. The error is written for the record, not
// for a log - it is the sentence the model reads in place of the contents.
func extractText(object api.EvidenceObject) (string, error) {
	body := io.LimitReader(object.Body, maxEvidenceBytes+1)
	switch object.ContentType {
	case "text/plain", "text/csv":
		raw, err := io.ReadAll(body)
		if err != nil {
			return "", fmt.Errorf("could not be fetched: %w", err)
		}
		if int64(len(raw)) > maxEvidenceBytes {
			return "", errors.New("larger than the upload limit")
		}
		if !utf8.Valid(raw) {
			return "", errors.New("not valid UTF-8 text")
		}
		return tidy(string(raw)), nil
	case "application/pdf":
		raw, err := io.ReadAll(body)
		if err != nil {
			return "", fmt.Errorf("could not be fetched: %w", err)
		}
		if int64(len(raw)) > maxEvidenceBytes {
			return "", errors.New("larger than the upload limit")
		}
		return pdfText(raw)
	case "image/png", "image/jpeg":
		return "", errors.New("an image; its contents are not extracted")
	default:
		return "", fmt.Errorf("content type %q is not read", object.ContentType)
	}
}

// pdfText reads the text layer of a PDF, page by page, up to the page cap.
//
// A scanned document has no text layer and comes back empty, which is
// reported as such rather than as an empty string: "this file says nothing"
// and "this file could not be read" lead a drafter to different letters. The
// parser is third-party code fed bytes an outsider uploaded, so a panic in it
// is a fact about the file, caught here, and never a fact about the worker.
func pdfText(raw []byte) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			text, err = "", fmt.Errorf("PDF could not be parsed: %v", r)
		}
	}()

	reader, err := pdf.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", fmt.Errorf("PDF could not be parsed: %w", err)
	}

	var b strings.Builder
	fonts := make(map[string]*pdf.Font)
	pages := reader.NumPage()
	for i := 1; i <= pages && i <= maxEvidencePages; i++ {
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}
		content, err := page.GetPlainText(fonts)
		if err != nil {
			return "", fmt.Errorf("PDF page %d could not be read: %w", i, err)
		}
		b.WriteString(content)
		b.WriteString("\n")
		if b.Len() > 4*maxEvidenceFileRunes {
			// More than the prompt can take, whatever the encoding; the rest
			// of the pages would be interpreted and thrown away.
			break
		}
	}

	out := tidy(b.String())
	if out == "" {
		if pages > maxEvidencePages {
			return "", fmt.Errorf("no text layer in the first %d pages; a scan needs OCR", maxEvidencePages)
		}
		return "", errors.New("no text layer; a scan needs OCR")
	}
	if pages > maxEvidencePages {
		out += fmt.Sprintf("\n[%d more pages were not read]", pages-maxEvidencePages)
	}
	return out, nil
}

// tidy makes extracted text cheap to read: control characters other than
// line breaks and tabs are dropped, trailing space goes, and runs of blank
// lines collapse to one. Nothing else changes, because the text is quoted
// back to a model that is told to copy figures exactly.
func tidy(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r == '\r' {
			return -1
		}
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)

	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
