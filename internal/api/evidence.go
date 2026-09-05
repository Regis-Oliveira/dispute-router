package api

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Evidence is the file store behind a dispute's representment.
//
// The files never pass through this service. The browser is given a presigned
// URL and talks to S3 directly, so a 40MB scan of a delivery receipt is not
// 40MB through a Go process, and the API stays free of the one workload that
// would force it to think about request body limits and memory.
type Evidence struct {
	client    *s3.Client
	presigner *s3.PresignClient
	bucket    string
	ttl       time.Duration
}

func NewEvidence(client *s3.Client, bucket string, ttl time.Duration) *Evidence {
	return &Evidence{
		client:    client,
		presigner: s3.NewPresignClient(client),
		bucket:    bucket,
		ttl:       ttl,
	}
}

// allowedTypes is a whitelist rather than a blacklist. Evidence is a document
// or an image; anything else is either a mistake or someone using the bucket
// as free hosting.
var allowedTypes = map[string]string{
	"application/pdf": ".pdf",
	"image/png":       ".png",
	"image/jpeg":      ".jpg",
	"text/plain":      ".txt",
	"text/csv":        ".csv",
}

// prefix keys every object under its dispute, which is what makes listing one
// dispute's evidence a prefix scan rather than a database table.
func (e *Evidence) prefix(disputeID int64) string {
	return fmt.Sprintf("disputes/%d/", disputeID)
}

// safeName strips a caller-supplied filename down to something that cannot
// escape its prefix.
//
// path.Base removes any directory part, so "../../secrets" becomes "secrets"
// and a key can only ever land under its own dispute. The remaining characters
// are then restricted, because a key is also a URL.
func safeName(name string) string {
	name = path.Base(strings.TrimSpace(name))
	if name == "." || name == "/" || name == "" {
		return ""
	}

	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	cleaned := strings.Trim(b.String(), "-.")
	if len(cleaned) > 120 {
		cleaned = cleaned[:120]
	}
	return cleaned
}

// MaxUploadBytes is enforced by S3 itself, not by this service and not by the
// browser.
const MaxUploadBytes int64 = 25 * 1024 * 1024

type UploadTarget struct {
	Key       string    `json:"key"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	// Fields go into the multipart form, before the file part. They carry the
	// signed policy; the browser cannot change any of them without invalidating
	// the signature.
	Fields map[string]string `json:"fields"`
	// MaxBytes is the same limit the policy encodes, repeated here only so the
	// client can refuse an oversized file without spending the upload first.
	MaxBytes    int64  `json:"max_bytes"`
	ContentType string `json:"content_type"`
}

// PresignUpload mints a signed policy the browser posts a form to.
//
// A presigned PUT cannot carry a size limit: its signature covers the method,
// the key and the content type, and nothing about the body. So a client that
// ignored the documented maximum could put a 4GB file in the bucket and the
// only defence was asking nicely.
//
// A presigned POST signs a *policy document* instead, and S3 enforces every
// condition in it - including content-length-range, which it checks against the
// actual bytes received. The limit stops being a request and becomes a rule.
func (e *Evidence) PresignUpload(ctx context.Context, disputeID int64, filename, contentType string) (UploadTarget, error) {
	if _, ok := allowedTypes[contentType]; !ok {
		return UploadTarget{}, fmt.Errorf("%w: content type %q is not accepted", ErrInvalidInput, contentType)
	}

	name := safeName(filename)
	if name == "" {
		return UploadTarget{}, fmt.Errorf("%w: filename is empty after sanitising", ErrInvalidInput)
	}

	// The timestamp keeps a re-upload of the same filename from silently
	// replacing the original. Evidence is not something to overwrite.
	key := fmt.Sprintf("%s%d-%s", e.prefix(disputeID), time.Now().UTC().Unix(), name)

	signed, err := e.presigner.PresignPostObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(e.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}, func(o *s3.PresignPostOptions) {
		o.Expires = e.ttl
		o.Conditions = []any{
			// The condition that could not be expressed as a PUT. S3 counts the
			// bytes it receives and refuses anything outside the range, so the
			// limit holds against a client that ignores every other signal.
			[]any{"content-length-range", 1, MaxUploadBytes},
			// Pinning the type stops a signed policy for a PDF being reused to
			// upload something else under the same key.
			map[string]string{"Content-Type": contentType},
		}
	})
	if err != nil {
		return UploadTarget{}, fmt.Errorf("presign upload: %w", err)
	}

	// Every condition in the policy needs a matching form field, and the SDK
	// does not add this one: PutObjectInput.ContentType shapes the policy but
	// not the returned Values. Without it S3 answers
	// "Policy Condition failed: {"Content-Type": ...}" on a perfectly valid
	// upload, which reads like a signing bug rather than a missing field.
	fields := make(map[string]string, len(signed.Values)+1)
	for name, value := range signed.Values {
		fields[name] = value
	}
	fields["Content-Type"] = contentType

	return UploadTarget{
		Key:         key,
		URL:         signed.URL,
		ExpiresAt:   time.Now().Add(e.ttl).UTC(),
		Fields:      fields,
		MaxBytes:    MaxUploadBytes,
		ContentType: contentType,
	}, nil
}

type EvidenceFile struct {
	Key        string    `json:"key"`
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	UploadedAt time.Time `json:"uploaded_at"`
	// URL is a short-lived presigned GET. Never a public link: evidence is a
	// customer's receipt, their address, sometimes their signature.
	URL string `json:"url"`
}

// List returns everything filed against a dispute.
func (e *Evidence) List(ctx context.Context, disputeID int64) ([]EvidenceFile, error) {
	out, err := e.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(e.bucket),
		Prefix: aws.String(e.prefix(disputeID)),
	})
	if err != nil {
		return nil, fmt.Errorf("list evidence: %w", err)
	}

	files := make([]EvidenceFile, 0, len(out.Contents))
	for _, object := range out.Contents {
		key := aws.ToString(object.Key)

		signed, err := e.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(e.bucket),
			Key:    aws.String(key),
		}, s3.WithPresignExpires(e.ttl))
		if err != nil {
			return nil, fmt.Errorf("presign download for %s: %w", key, err)
		}

		files = append(files, EvidenceFile{
			Key:        key,
			Name:       path.Base(key),
			SizeBytes:  aws.ToInt64(object.Size),
			UploadedAt: aws.ToTime(object.LastModified),
			URL:        signed.URL,
		})
	}
	return files, nil
}
