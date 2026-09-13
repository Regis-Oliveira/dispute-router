// Package config loads runtime settings from the environment.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/regisoliveira/dispute-router/internal/awsx"
)

// Config is every setting the binaries read, with the environment as the only
// source.
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

	// The representment assistant. Every run reaches a real model and costs
	// money - which is why the ceilings are configuration rather than
	// constants.
	//
	// A key from console.anthropic.com. A claude.ai subscription is a separate
	// product and cannot be used here. There was a Bedrock provider beside
	// this one, for a deployment where the ECS task role would supply the
	// credential; it never executed - there is no account behind this project
	// - and was removed rather than kept as untested code. The argument for it
	// is in docs/DECISIONS.md.
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

	// AgentMaxCostMicros bounds one dispute across the generator and the
	// verifier, in micro-dollars. An automation that costs more than the
	// chargeback it works on has inverted its own business case.
	AgentMaxCostMicros int64
	AgentMaxAttempts   int
	AgentBatchSize     int
	AgentInputPerMTok  int64
	AgentOutputPerMTok int64

	// PprofAddr serves the runtime's diagnostics (profiles, goroutine dumps,
	// the execution trace) on its own loopback listener. Empty is off. Each
	// binary has its own default port so all three can be on at once:
	// ingest 6062, api 6060, worker 6061.
	PprofAddr string

	// Read API.
	APIAddr        string
	CORSOrigins    []string
	RequestTimeout time.Duration

	ShutdownTimeout time.Duration
}

// Load reads .env (if present) and then the environment, which wins.
func Load() (Config, error) {
	if err := loadDotEnv(dotenvPath()); err != nil {
		return Config{}, err
	}

	var vars env
	cfg := Config{
		Addr:                 str("INGEST_ADDR", ":8080"),
		DatabaseURL:          str("DATABASE_URL", ""),
		RedisURL:             str("REDIS_URL", "redis://localhost:6379"),
		IdempotencyTTL:       vars.dur("INGEST_IDEMPOTENCY_TTL", 24*time.Hour),
		SignatureTolerance:   vars.dur("INGEST_SIGNATURE_TOLERANCE", 5*time.Minute),
		RateLimitPerMinute:   vars.integer("INGEST_RATE_PER_MINUTE", 600),
		RateLimitBurst:       vars.integer("INGEST_RATE_BURST", 120),
		IPRateLimitPerMinute: vars.integer("INGEST_IP_RATE_PER_MINUTE", 1200),
		IPRateLimitBurst:     vars.integer("INGEST_IP_RATE_BURST", 240),
		MaxBodyBytes:         int64(vars.integer("INGEST_MAX_BODY_BYTES", 64*1024)),
		OutboxPollInterval:   vars.dur("INGEST_OUTBOX_POLL", time.Second),
		OutboxBatchSize:      vars.integer("INGEST_OUTBOX_BATCH", 100),
		WorkerConcurrency:    vars.integer("WORKER_CONCURRENCY", 8),
		WorkerPollInterval:   vars.dur("WORKER_POLL_INTERVAL", 2*time.Second),
		WorkerBatchSize:      vars.integer("WORKER_BATCH_SIZE", 100),
		// Claim work slightly before it is due, so a decision lands inside the
		// window instead of exactly on its edge.
		WorkerLookahead: vars.dur("WORKER_LOOKAHEAD", 30*time.Second),
		// Long enough that it is not constant load, short enough that a gap
		// left by an unreachable Redis is closed well before any deadline.
		WorkerReconcileInterval: vars.dur("WORKER_RECONCILE_INTERVAL", 60*time.Second),
		// Comfortably longer than one decision takes. A lock that expires
		// mid-decision is survivable - the version check catches it - but it
		// wastes the work.
		WorkerLockTTL:       vars.dur("WORKER_LOCK_TTL", 30*time.Second),
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
		WebhookSecretTTL: vars.dur("WEBHOOK_SECRET_TTL", 5*time.Minute),
		SQSMaxMessages:   vars.integer("SQS_MAX_MESSAGES", 10),
		SQSWaitSeconds:   vars.integer("SQS_WAIT_SECONDS", 20),

		AnthropicAPIKey:    str("ANTHROPIC_API_KEY", ""),
		AnthropicModel:     str("ANTHROPIC_MODEL", "claude-sonnet-5"),
		VoyageAPIKey:       str("VOYAGE_API_KEY", ""),
		VoyageModel:        str("VOYAGE_MODEL", "voyage-4"),
		PrecedentLimit:     vars.integer("PRECEDENT_LIMIT", 3),
		AgentMaxCostMicros: int64(vars.integer("AGENT_MAX_COST_MICROS", 250_000)),
		AgentMaxAttempts:   vars.integer("AGENT_MAX_ATTEMPTS", 2),
		AgentBatchSize:     vars.integer("AGENT_BATCH_SIZE", 5),
		AgentInputPerMTok:  int64(vars.integer("AGENT_INPUT_MICROS_PER_MTOK", 3_000_000)),
		AgentOutputPerMTok: int64(vars.integer("AGENT_OUTPUT_MICROS_PER_MTOK", 15_000_000)),

		PprofAddr: str("PPROF_ADDR", ""),
		APIAddr:   str("API_ADDR", ":8081"),
		// Named origins only. A reflected Origin or a bare "*" would let any
		// page on the internet read this data out of an operator's browser.
		CORSOrigins:    list("API_CORS_ORIGINS", []string{"http://localhost:4200"}),
		RequestTimeout: vars.dur("API_REQUEST_TIMEOUT", 20*time.Second),

		ShutdownTimeout: vars.dur("INGEST_SHUTDOWN_TIMEOUT", 15*time.Second),
	}
	if err := errors.Join(vars.errs...); err != nil {
		return Config{}, err
	}

	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if cfg.RateLimitBurst <= 0 || cfg.RateLimitPerMinute <= 0 {
		return Config{}, errors.New("rate limit settings must be positive")
	}
	// A missing queue URL surfaced as an EC2 metadata timeout three layers
	// away, because an empty endpoint sends the SDK looking for a real AWS and
	// an instance role. Settings that cannot work are refused here instead.
	if cfg.SQSQueueURL == "" {
		return Config{}, errors.New("SQS_QUEUE_URL is required")
	}
	if cfg.S3EvidenceBucket == "" {
		return Config{}, errors.New("S3_EVIDENCE_BUCKET is required")
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

// env collects parse failures so Load can report every bad variable at once
// instead of the first one per restart.
//
// A value that is present but unparseable is an error, not the fallback:
// WORKER_LOCK_TTL=30 used to become the 30 s default by accident, which is the
// one case where silently agreeing with the operator hides the mistake.
type env struct {
	errs []error
}

func (e *env) integer(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", key, err))
		return fallback
	}
	return n
}

func (e *env) dur(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", key, err))
		return fallback
	}
	return d
}

// AWS is the part of the configuration awsx.Load needs, mapped in one place so
// no binary can forget a field.
func (c Config) AWS() awsx.Config {
	return awsx.Config{
		Region:          c.AWSRegion,
		Endpoint:        c.AWSEndpoint,
		AccessKeyID:     c.AWSAccessKey,
		SecretAccessKey: c.AWSSecretKey,
	}
}

// dotenvPath is DOTENV_PATH when set, else the repo-root .env relative to the
// working directory. The Makefile runs every binary from the root; an MCP
// client starts cmd/mcp with an unpredictable working directory, which is why
// the path is configurable and the client config passes it explicitly.
func dotenvPath() string {
	if explicit := os.Getenv("DOTENV_PATH"); explicit != "" {
		return explicit
	}
	return ".env"
}

// loadDotEnv reads the same repo-root .env the Node simulator reads, so both
// halves of the webhook contract are configured from one file. Values already
// present in the environment are left alone.
func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
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
