package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// Embedder turns text into a vector. The same seam as Completer, for the same
// reason: the thing worth testing is what surrounds the call, and every real
// call costs money.
//
// Anthropic does not serve embeddings, so this is a second provider whatever
// happens. Voyage is the one Anthropic points at, and voyage-3 is 1024
// dimensions - which is baked into the schema, so changing model here is a
// migration and not a config change.
type Embedder interface {
	// Embed returns one vector per input, in order. Batched because the cost
	// of a request dwarfs the cost of a row, and the backfill has thousands.
	Embed(ctx context.Context, texts []string, kind EmbedKind) ([][]float32, error)
	Model() string
	Dimensions() int
}

// EmbedKind is the asymmetry most embedding APIs expose and most callers
// ignore. A stored document and a search query are embedded with different
// prefixes, and mixing them up costs recall quietly - the results are still
// plausible, just worse, which is the hardest kind of regression to notice.
type EmbedKind string

const (
	EmbedDocument EmbedKind = "document"
	EmbedQuery    EmbedKind = "query"
)

// ---------------------------------------------------------------------------
// Voyage
// ---------------------------------------------------------------------------

const (
	voyageEndpoint = "https://api.voyageai.com/v1/embeddings"
	// The width the schema declares. Asserted rather than assumed: a model
	// returning 512 would be silently rejected by the column, and the error
	// would name the column rather than the mistake.
	voyageDimensions = 1024
	// Voyage caps a request; beyond it the batch has to be split.
	voyageMaxBatch = 128
)

type Voyage struct {
	client *http.Client
	key    string
	model  string
	// pause is how long to wait after a rate limit. A field so a test does not
	// have to sit through a real one.
	pause    time.Duration
	endpoint string
}

func NewVoyage(key, model string) (*Voyage, error) {
	if key == "" {
		return nil, errors.New("agent: VOYAGE_API_KEY is required for embeddings " +
			"(Anthropic does not serve embeddings; without a key the retriever falls back to full-text search)")
	}
	if model == "" {
		model = "voyage-4"
	}
	return &Voyage{
		client:   &http.Client{Timeout: 2 * time.Minute},
		key:      key,
		model:    model,
		endpoint: voyageEndpoint,
		// Voyage limits requests per MINUTE - three of them on an account with
		// no payment method. A backoff measured in seconds cannot clear a
		// window measured in minutes, so it retries forever and fails anyway.
		// This waits out the window instead.
		pause: 25 * time.Second,
	}, nil
}

func (v *Voyage) Model() string   { return v.model }
func (v *Voyage) Dimensions() int { return voyageDimensions }

type voyageRequest struct {
	Input     []string `json:"input"`
	Model     string   `json:"model"`
	InputType string   `json:"input_type"`

	// The width is requested, not hoped for.
	//
	// The schema declares vector(1024), and asking the API for exactly that
	// turns a mismatch into an error from the model provider naming the
	// parameter, instead of a surprise discovered after a backfill has run.
	// Models that do not support the parameter will say so, which is also an
	// answer and a cheap one.
	OutputDimension int `json:"output_dimension,omitempty"`
}

type voyageResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

func (v *Voyage) Embed(ctx context.Context, texts []string, kind EmbedKind) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))

	for start := 0; start < len(texts); start += voyageMaxBatch {
		end := min(start+voyageMaxBatch, len(texts))

		vectors, err := v.withRetry(ctx, texts[start:end], kind)
		if err != nil {
			return nil, err
		}
		out = append(out, vectors...)
	}
	return out, nil
}

// errRateLimited marks the one failure worth waiting out rather than giving up
// on. Everything else is either permanent or transient in seconds.
var errRateLimited = errors.New("rate limited")

func (v *Voyage) withRetry(ctx context.Context, texts []string, kind EmbedKind) ([][]float32, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		vectors, err := v.batch(ctx, texts, kind)
		if err == nil {
			return vectors, nil
		}
		lastErr = err
		if !errors.Is(err, errRateLimited) || attempt == maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(v.pause):
		}
	}
	return nil, lastErr
}

func (v *Voyage) batch(ctx context.Context, texts []string, kind EmbedKind) ([][]float32, error) {
	body, err := json.Marshal(voyageRequest{
		Input:           texts,
		Model:           v.model,
		InputType:       string(kind),
		OutputDimension: voyageDimensions,
	})
	if err != nil {
		return nil, fmt.Errorf("voyage: encoding request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("voyage: building request: %w", err)
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("authorization", "Bearer "+v.key)

	res, err := v.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("voyage: %w", err)
	}
	defer res.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("voyage: reading response: %w", err)
	}
	if res.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("voyage: %w: %s", errRateLimited, bytes.TrimSpace(payload))
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("voyage: %s: %s", res.Status, bytes.TrimSpace(payload))
	}

	var decoded voyageResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("voyage: decoding response: %w", err)
	}
	if len(decoded.Data) != len(texts) {
		return nil, fmt.Errorf("voyage: asked for %d embeddings, got %d", len(texts), len(decoded.Data))
	}

	// Returned in an "index" field rather than in order, and the field exists
	// precisely because the order is not guaranteed. Placing them by index is
	// the difference between a correct index and one whose neighbours are
	// somebody else's.
	vectors := make([][]float32, len(texts))
	for _, item := range decoded.Data {
		if item.Index < 0 || item.Index >= len(texts) {
			return nil, fmt.Errorf("voyage: response index %d is outside the batch", item.Index)
		}
		if len(item.Embedding) != voyageDimensions {
			return nil, fmt.Errorf("voyage: model %s returned %d dimensions, the schema declares %d",
				v.model, len(item.Embedding), voyageDimensions)
		}
		vectors[item.Index] = normalise(item.Embedding)
	}
	for i, vec := range vectors {
		if vec == nil {
			return nil, fmt.Errorf("voyage: no embedding came back for input %d", i)
		}
	}
	return vectors, nil
}

// normalise makes every vector unit length, so cosine distance and inner
// product agree and the index only has to compare direction. Voyage already
// returns normalised vectors; doing it again is cheap and means the invariant
// holds regardless of who supplied them.
func normalise(vec []float32) []float32 {
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		return vec
	}
	norm := float32(math.Sqrt(sum))
	out := make([]float32, len(vec))
	for i, v := range vec {
		out[i] = v / norm
	}
	return out
}

// pgvector renders a slice in the literal form the extension parses. pgx has no
// native codec for it without a registration, and a string is unambiguous.
func pgvector(vec []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", v)
	}
	b.WriteByte(']')
	return b.String()
}
