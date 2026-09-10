// Package config loads runtime settings from the environment.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr        string
	DatabaseURL string
	RedisURL    string

	// How long a processed idempotency key stays in Redis. The unique index on
	// webhook_events.idempotency_key is the durable backstop, so this only has
	// to outlive a sender's retry window - not forever.
	IdempotencyTTL time.Duration

	// Signatures older than this are rejected without hashing anything, which
	// is what stops a captured request from replaying a week later.
	SignatureTolerance time.Duration

	// Per-merchant token bucket. Deliberately per-merchant: one shared bucket
	// means a single noisy sender can rate-limit everybody else off the
	// platform.
	RateLimitPerMinute int
	RateLimitBurst     int

	// Coarser pre-authentication limit, keyed by remote address. Cheap
	// protection for the work done before a signature has been verified.
	IPRateLimitPerMinute int
	IPRateLimitBurst     int

	MaxBodyBytes int64

	OutboxPollInterval time.Duration
	OutboxBatchSize    int

	// Deadline worker.
	WorkerConcurrency       int
	WorkerPollInterval      time.Duration
	WorkerBatchSize         int
	WorkerLookahead         time.Duration
	WorkerReconcileInterval time.Duration
	WorkerLockTTL           time.Duration

	// AWS. Endpoint is LocalStack; empty means the real thing.
	AWSRegion        string
	AWSEndpoint      string
	AWSAccessKey     string
	AWSSecretKey     string
	SQSQueueURL      string
	SQSDLQURL        string
	S3EvidenceBucket string
	// WebhookSecretSource is "secretsmanager" or "database". The database is
	// the fallback that keeps the system runnable with no AWS at all.
	WebhookSecretSource string
	WebhookSecretID     string
	WebhookSecretTTL    time.Duration
	SQSMaxMessages      int
	SQSWaitSeconds      int

	// The representment assistant. Bedrock is not emulated by LocalStack, so
	// unlike every other AWS client here this one always reaches the real
	// thing and every run costs money - which is why the ceilings are
	// configuration rather than constants.
	// ModelProvider is "anthropic" or "bedrock".
	//
	// Two, because they answer different situations: Bedrock needs an AWS
	// account with model access and gets its credentials from the task role,
	// which is the right shape in production; the direct API needs a key and
	// nothing else, which is the only shape available to somebody trying this
	// on a laptop.
	ModelProvider string

	// A key from console.anthropic.com. A claude.ai subscription is a separate
	// product and cannot be used here.
	AnthropicAPIKey string
	AnthropicModel  string

	// Embeddings for precedent retrieval. Anthropic does not serve embeddings,
	// so this is a second provider whatever happens. Without a key the
	// retriever falls back to full-text search, which is the baseline the
	// vector path has to beat anyway.
	VoyageAPIKey string
	VoyageModel  string
	// How many precedents reach the prompt. Small: each one is another case the
	// drafter could borrow a figure from, and the risk grows faster than the
	// signal.
	PrecedentLimit int

	BedrockModelID string
	// AgentMaxCostMicros bounds one dispute across the generator and the
	// verifier, in micro-dollars. An automation that costs more than the
	// chargeback it works on has inverted its own business case.
	AgentMaxCostMicros int64
	AgentMaxAttempts   int
	AgentBatchSize     int
	AgentInputPerMTok  int64
	AgentOutputPerMTok int64

	// Read API.
	APIAddr        string
	CORSOrigins    []string
	RequestTimeout time.Duration

	ShutdownTimeout time.Duration
}

// Load reads .env (if present) and then the environment, which wins.
func Load(dotenvPath string) (Config, error) {
	if err := loadDotEnv(dotenvPath); err != nil {
		return Config{}, err
	}

	cfg := Config{
		Addr:                 str("INGEST_ADDR", ":8080"),
		DatabaseURL:          str("DATABASE_URL", ""),
		RedisURL:             str("REDIS_URL", "redis://localhost:6379"),
		IdempotencyTTL:       dur("INGEST_IDEMPOTENCY_TTL", 24*time.Hour),
		SignatureTolerance:   dur("INGEST_SIGNATURE_TOLERANCE", 5*time.Minute),
		RateLimitPerMinute:   integer("INGEST_RATE_PER_MINUTE", 600),
		RateLimitBurst:       integer("INGEST_RATE_BURST", 120),
		IPRateLimitPerMinute: integer("INGEST_IP_RATE_PER_MINUTE", 1200),
		IPRateLimitBurst:     integer("INGEST_IP_RATE_BURST", 240),
		MaxBodyBytes:         int64(integer("INGEST_MAX_BODY_BYTES", 64*1024)),
		OutboxPollInterval:   dur("INGEST_OUTBOX_POLL", time.Second),
		OutboxBatchSize:      integer("INGEST_OUTBOX_BATCH", 100),
		WorkerConcurrency:    integer("WORKER_CONCURRENCY", 8),
		WorkerPollInterval:   dur("WORKER_POLL_INTERVAL", 2*time.Second),
		WorkerBatchSize:      integer("WORKER_BATCH_SIZE", 100),
		// Claim work slightly before it is due, so a decision lands inside the
		// window instead of exactly on its edge.
		WorkerLookahead: dur("WORKER_LOOKAHEAD", 30*time.Second),
		// Long enough that it is not constant load, short enough that a gap
		// left by an unreachable Redis is closed well before any deadline.
		WorkerReconcileInterval: dur("WORKER_RECONCILE_INTERVAL", 60*time.Second),
		// Comfortably longer than one decision takes. A lock that expires
		// mid-decision is survivable - the version check catches it - but it
		// wastes the work.
		WorkerLockTTL:       dur("WORKER_LOCK_TTL", 30*time.Second),
		AWSRegion:           str("AWS_REGION", "us-east-1"),
		AWSEndpoint:         str("AWS_ENDPOINT_URL", "http://localhost:4566"),
		AWSAccessKey:        str("AWS_ACCESS_KEY_ID", "test"),
		AWSSecretKey:        str("AWS_SECRET_ACCESS_KEY", "test"),
		SQSQueueURL:         str("SQS_QUEUE_URL", "http://localhost:4566/000000000000/disputes-events"),
		SQSDLQURL:           str("SQS_DLQ_URL", "http://localhost:4566/000000000000/disputes-events-dlq"),
		S3EvidenceBucket:    str("S3_EVIDENCE_BUCKET", "dispute-evidence"),
		WebhookSecretSource: str("WEBHOOK_SECRET_SOURCE", "secretsmanager"),
		WebhookSecretID:     str("WEBHOOK_SECRET_ID", "dispute-router/webhook-secrets"),
		// The cache TTL is the real rotation latency: a key published now takes
		// effect within this window. Minutes, not hours.
		WebhookSecretTTL: dur("WEBHOOK_SECRET_TTL", 5*time.Minute),
		SQSMaxMessages:   integer("SQS_MAX_MESSAGES", 10),
		SQSWaitSeconds:   integer("SQS_WAIT_SECONDS", 20),

		ModelProvider:      str("MODEL_PROVIDER", "anthropic"),
		AnthropicAPIKey:    str("ANTHROPIC_API_KEY", ""),
		AnthropicModel:     str("ANTHROPIC_MODEL", "claude-sonnet-5"),
		VoyageAPIKey:       str("VOYAGE_API_KEY", ""),
		VoyageModel:        str("VOYAGE_MODEL", "voyage-3"),
		PrecedentLimit:     integer("PRECEDENT_LIMIT", 3),
		BedrockModelID:     str("BEDROCK_MODEL_ID", ""),
		AgentMaxCostMicros: int64(integer("AGENT_MAX_COST_MICROS", 250_000)),
		AgentMaxAttempts:   integer("AGENT_MAX_ATTEMPTS", 2),
		AgentBatchSize:     integer("AGENT_BATCH_SIZE", 5),
		AgentInputPerMTok:  int64(integer("AGENT_INPUT_MICROS_PER_MTOK", 3_000_000)),
		AgentOutputPerMTok: int64(integer("AGENT_OUTPUT_MICROS_PER_MTOK", 15_000_000)),

		APIAddr: str("API_ADDR", ":8081"),
		// Named origins only. A reflected Origin or a bare "*" would let any
		// page on the internet read this data out of an operator's browser.
		CORSOrigins:    list("API_CORS_ORIGINS", []string{"http://localhost:4200"}),
		RequestTimeout: dur("API_REQUEST_TIMEOUT", 20*time.Second),

		ShutdownTimeout: dur("INGEST_SHUTDOWN_TIMEOUT", 15*time.Second),
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.RateLimitBurst <= 0 || cfg.RateLimitPerMinute <= 0 {
		return Config{}, fmt.Errorf("rate limit settings must be positive")
	}
	// A missing queue URL surfaced as an EC2 metadata timeout three layers
	// away, because an empty endpoint sends the SDK looking for a real AWS and
	// an instance role. Settings that cannot work are refused here instead.
	if cfg.SQSQueueURL == "" {
		return Config{}, fmt.Errorf("SQS_QUEUE_URL is required")
	}
	if cfg.S3EvidenceBucket == "" {
		return Config{}, fmt.Errorf("S3_EVIDENCE_BUCKET is required")
	}
	switch cfg.ModelProvider {
	case "anthropic", "bedrock":
	default:
		return Config{}, fmt.Errorf("MODEL_PROVIDER must be anthropic or bedrock, got %q", cfg.ModelProvider)
	}
	switch cfg.WebhookSecretSource {
	case "secretsmanager", "database":
	default:
		return Config{}, fmt.Errorf("WEBHOOK_SECRET_SOURCE must be secretsmanager or database, got %q", cfg.WebhookSecretSource)
	}
	return cfg, nil
}

func str(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func list(key string, fallback []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func integer(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func dur(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

// loadDotEnv reads the same repo-root .env the Node simulator reads, so both
// halves of the webhook contract are configured from one file. Values already
// present in the environment are left alone.
func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
