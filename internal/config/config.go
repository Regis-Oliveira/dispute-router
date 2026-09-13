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
//
// The fields here are the ones more than one binary reads; everything else
// sits in the group named after its consumer, so that a main touches one
// sub-struct and a reader can tell from the type which process a knob belongs
// to. Grouping is the only thing that changed: the environment variable names
// and their meanings are unchanged.
type Config struct {
	DatabaseURL string
	RedisURL    string

	// PprofAddr serves the runtime's diagnostics (profiles, goroutine dumps,
	// the execution trace) on its own loopback listener. Empty is off. Each
	// binary has its own default port so all three can be on at once:
	// ingest 6062, api 6060, worker 6061.
	PprofAddr string

	ShutdownTimeout time.Duration

	Ingest Ingest
	Worker Worker
	Agent  Agent
	API    API
	AWS    AWS
	Sentry Sentry
}

// Ingest is what cmd/ingest reads: the webhook endpoint, its defences, and the
// outbox relay that runs beside it.
type Ingest struct {
	Addr string

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

	// WebhookSecretSource is "secretsmanager" or "database". The database is
	// the fallback that keeps the system runnable with no AWS at all.
	WebhookSecretSource string
	WebhookSecretID     string
	WebhookSecretTTL    time.Duration
}

// Worker is what cmd/worker reads: how the deadline pool claims work and how
// long it holds a lock while it decides.
type Worker struct {
	Concurrency  int
	PollInterval time.Duration
	BatchSize    int
	// Lookahead claims work slightly before it is due, so a decision lands
	// inside the window instead of exactly on its edge.
	Lookahead time.Duration
	// ReconcileInterval is long enough that it is not constant load, short
	// enough that a gap left by an unreachable Redis is closed well before any
	// deadline.
	ReconcileInterval time.Duration
	// LockTTL is comfortably longer than one decision takes. A lock that
	// expires mid-decision is survivable - the version check catches it - but
	// it wastes the work.
	LockTTL time.Duration
}

// Agent is what the model-facing commands read (cmd/agent, cmd/ask, cmd/eval,
// cmd/embed, cmd/retrieval).
//
// Every run reaches a real model and costs money - which is why the ceilings
// are configuration rather than constants.
type Agent struct {
	// AnthropicAPIKey is a key from console.anthropic.com. A claude.ai
	// subscription is a separate product and cannot be used here. There was a
	// Bedrock provider beside this one, for a deployment where the ECS task
	// role would supply the credential; it never executed - there is no
	// account behind this project - and was removed rather than kept as
	// untested code. The argument for it is in docs/DECISIONS.md.
	AnthropicAPIKey string
	AnthropicModel  string

	// Embeddings for precedent retrieval. Anthropic does not serve embeddings,
	// so this is a second provider whatever happens. Without a key the
	// retriever falls back to full-text search, which is the baseline the
	// vector path has to beat anyway.
	VoyageAPIKey string
	VoyageModel  string

	// PrecedentLimit is how many precedents reach the prompt. Small: each one
	// is another case the drafter could borrow a figure from, and the risk
	// grows faster than the signal.
	PrecedentLimit int

	// MaxCostMicros bounds one dispute across the generator and the verifier,
	// in micro-dollars. An automation that costs more than the chargeback it
	// works on has inverted its own business case.
	MaxCostMicros int64
	MaxAttempts   int
	BatchSize     int
	InputPerMTok  int64
	OutputPerMTok int64
}

// API is what cmd/api reads: the read-only HTTP surface the dashboard calls.
type API struct {
	Addr string
	// Named origins only. A reflected Origin or a bare "*" would let any page
	// on the internet read this data out of an operator's browser.
	CORSOrigins    []string
	RequestTimeout time.Duration
}

// AWS is where AWS is, plus the names of the resources this platform uses
// there.
//
// The embedded awsx.Config is handed to awsx.Load as it stands, so there is no
// field-by-field mapping for a binary to get wrong - which is what the six
// hand-written awsx.Config literals this replaced kept getting wrong.
//
// Every field here has a LocalStack default, deliberately: an empty endpoint
// sends the SDK looking for a real AWS and an instance role, and a missing
// queue URL then surfaced as an EC2 metadata timeout three layers away. The
// settings that cannot work are avoided by defaulting to the ones that do,
// which is also why there is no AWS entry in RequireDatabase's family - none
// of these can be absent.
type AWS struct {
	awsx.Config

	QueueURL       string
	DLQURL         string
	EvidenceBucket string

	MaxMessages int
	WaitSeconds int
}

// Sentry is where a crash gets reported, for every binary that has a logger.
//
// The DSN is the switch. Empty - the default, and what every laptop runs with
// - means the SDK is never initialised and nothing leaves the process. There
// is no second "enabled" flag, because two ways to say off is one way to get
// it wrong.
type Sentry struct {
	// DSN is the endpoint out of the Sentry project's own settings. It is a
	// credential only in the weak sense, in that it permits writing events and
	// nothing else, but it is still not something to commit - which is why
	// .env.example carries the name and no value.
	//
	// A DSN that is set but malformed is a startup error rather than a silent
	// disabling, in line with the rule the rest of this package follows. The
	// check is in boot.Sentry: the string's grammar belongs to the SDK, and
	// this package does not import it.
	DSN string

	// Environment separates the events one deployment produces from another's.
	// "local" is the honest answer for a laptop, and it is the answer that
	// makes a laptop's events obvious the day there is a second source.
	Environment string

	// Release names the build an event came from, which is what makes "this
	// started on Tuesday" a question with an answer. Empty lets the SDK read
	// the VCS revision the Go toolchain stamps into a built binary; `go run`
	// leaves no stamp, so this is worth setting by hand as soon as there are
	// two builds to tell apart.
	Release string
}

// RequireDatabase reports whether DATABASE_URL is set, for the nine binaries
// that open a pool to say so themselves.
//
// It is a method rather than a check inside Load because the answer is per
// binary: cmd/dlq moves messages between two SQS queues and never opens a
// pool, yet used to refuse to start without a database it never touches.
func (c Config) RequireDatabase() error {
	if c.DatabaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	return nil
}

// Load reads .env (if present) and then the environment, which wins.
//
// What it checks is what is wrong whoever reads it: a value that is present
// but unparseable, and a value outside the range its field allows. Whether an
// empty setting is fatal depends on the binary, so that question is
// RequireDatabase's.
func Load() (Config, error) {
	dotenv, err := readDotEnv(dotenvPath())
	if err != nil {
		return Config{}, err
	}
	vars := env{dotenv: dotenv}

	cfg := Config{
		DatabaseURL:     vars.str("DATABASE_URL", ""),
		RedisURL:        vars.str("REDIS_URL", "redis://localhost:6379"),
		PprofAddr:       vars.str("PPROF_ADDR", ""),
		ShutdownTimeout: vars.dur("INGEST_SHUTDOWN_TIMEOUT", 15*time.Second),

		Ingest: Ingest{
			Addr:                 vars.str("INGEST_ADDR", ":8080"),
			IdempotencyTTL:       vars.dur("INGEST_IDEMPOTENCY_TTL", 24*time.Hour),
			SignatureTolerance:   vars.dur("INGEST_SIGNATURE_TOLERANCE", 5*time.Minute),
			RateLimitPerMinute:   vars.integer("INGEST_RATE_PER_MINUTE", 600),
			RateLimitBurst:       vars.integer("INGEST_RATE_BURST", 120),
			IPRateLimitPerMinute: vars.integer("INGEST_IP_RATE_PER_MINUTE", 1200),
			IPRateLimitBurst:     vars.integer("INGEST_IP_RATE_BURST", 240),
			MaxBodyBytes:         int64(vars.integer("INGEST_MAX_BODY_BYTES", 64*1024)),
			OutboxPollInterval:   vars.dur("INGEST_OUTBOX_POLL", time.Second),
			OutboxBatchSize:      vars.integer("INGEST_OUTBOX_BATCH", 100),
			WebhookSecretSource:  vars.str("WEBHOOK_SECRET_SOURCE", "secretsmanager"),
			WebhookSecretID:      vars.str("WEBHOOK_SECRET_ID", "dispute-router/webhook-secrets"),
			// The cache TTL is the real rotation latency: a key published now
			// takes effect within this window. Minutes, not hours.
			WebhookSecretTTL: vars.dur("WEBHOOK_SECRET_TTL", 5*time.Minute),
		},

		Worker: Worker{
			Concurrency:       vars.integer("WORKER_CONCURRENCY", 8),
			PollInterval:      vars.dur("WORKER_POLL_INTERVAL", 2*time.Second),
			BatchSize:         vars.integer("WORKER_BATCH_SIZE", 100),
			Lookahead:         vars.dur("WORKER_LOOKAHEAD", 30*time.Second),
			ReconcileInterval: vars.dur("WORKER_RECONCILE_INTERVAL", 60*time.Second),
			LockTTL:           vars.dur("WORKER_LOCK_TTL", 30*time.Second),
		},

		Agent: Agent{
			AnthropicAPIKey: vars.str("ANTHROPIC_API_KEY", ""),
			AnthropicModel:  vars.str("ANTHROPIC_MODEL", "claude-sonnet-5"),
			VoyageAPIKey:    vars.str("VOYAGE_API_KEY", ""),
			VoyageModel:     vars.str("VOYAGE_MODEL", "voyage-4"),
			PrecedentLimit:  vars.integer("PRECEDENT_LIMIT", 3),
			MaxCostMicros:   int64(vars.integer("AGENT_MAX_COST_MICROS", 250_000)),
			MaxAttempts:     vars.integer("AGENT_MAX_ATTEMPTS", 2),
			BatchSize:       vars.integer("AGENT_BATCH_SIZE", 5),
			InputPerMTok:    int64(vars.integer("AGENT_INPUT_MICROS_PER_MTOK", 3_000_000)),
			OutputPerMTok:   int64(vars.integer("AGENT_OUTPUT_MICROS_PER_MTOK", 15_000_000)),
		},

		API: API{
			Addr:           vars.str("API_ADDR", ":8081"),
			CORSOrigins:    vars.list("API_CORS_ORIGINS", []string{"http://localhost:4200"}),
			RequestTimeout: vars.dur("API_REQUEST_TIMEOUT", 20*time.Second),
		},

		AWS: AWS{
			Config: awsx.Config{
				Region:          vars.str("AWS_REGION", "us-east-1"),
				Endpoint:        vars.str("AWS_ENDPOINT_URL", "http://localhost:4566"),
				AccessKeyID:     vars.str("AWS_ACCESS_KEY_ID", "test"),
				SecretAccessKey: vars.str("AWS_SECRET_ACCESS_KEY", "test"),
			},
			QueueURL:       vars.str("SQS_QUEUE_URL", "http://localhost:4566/000000000000/disputes-events"),
			DLQURL:         vars.str("SQS_DLQ_URL", "http://localhost:4566/000000000000/disputes-events-dlq"),
			EvidenceBucket: vars.str("S3_EVIDENCE_BUCKET", "dispute-evidence"),
			MaxMessages:    vars.integer("SQS_MAX_MESSAGES", 10),
			WaitSeconds:    vars.integer("SQS_WAIT_SECONDS", 20),
		},

		Sentry: Sentry{
			DSN:         vars.str("SENTRY_DSN", ""),
			Environment: vars.str("SENTRY_ENVIRONMENT", "local"),
			Release:     vars.str("SENTRY_RELEASE", ""),
		},
	}
	if err := errors.Join(vars.errs...); err != nil {
		return Config{}, err
	}

	if cfg.Ingest.RateLimitBurst <= 0 || cfg.Ingest.RateLimitPerMinute <= 0 {
		return Config{}, errors.New("rate limit settings must be positive")
	}
	switch cfg.Ingest.WebhookSecretSource {
	case "secretsmanager", "database":
	default:
		return Config{}, fmt.Errorf("WEBHOOK_SECRET_SOURCE must be secretsmanager or database, got %q", cfg.Ingest.WebhookSecretSource)
	}
	return cfg, nil
}

// env resolves one variable at a time against the process environment and the
// parsed .env, and collects parse failures so Load can report every bad
// variable at once instead of the first one per restart.
//
// A value that is present but unparseable is an error, not the fallback:
// WORKER_LOCK_TTL=30 used to become the 30 s default by accident, which is the
// one case where silently agreeing with the operator hides the mistake.
type env struct {
	dotenv map[string]string
	errs   []error
}

// lookup is the process environment first, then .env. A variable present in
// the process environment shadows the file even when it is empty, which is the
// rule the os.Setenv pass this replaced enforced by never overwriting. An
// empty value is "not set", so the caller's fallback applies.
func (e *env) lookup(key string) (string, bool) {
	if v, ok := os.LookupEnv(key); ok {
		return v, v != ""
	}
	v := e.dotenv[key]
	return v, v != ""
}

func (e *env) str(key, fallback string) string {
	if v, ok := e.lookup(key); ok {
		return v
	}
	return fallback
}

func (e *env) list(key string, fallback []string) []string {
	v, ok := e.lookup(key)
	if !ok {
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

func (e *env) integer(key string, fallback int) int {
	v, ok := e.lookup(key)
	if !ok {
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
	v, ok := e.lookup(key)
	if !ok {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", key, err))
		return fallback
	}
	return d
}

// dotenvPath is DOTENV_PATH when set, else the repo-root .env relative to the
// working directory. The Makefile runs every binary from the root; an MCP
// client starts cmd/mcp with an unpredictable working directory, which is why
// the path is configurable and the client config passes it explicitly.
//
// It reads the process environment directly: a .env cannot say where it is.
func dotenvPath() string {
	if explicit := os.Getenv("DOTENV_PATH"); explicit != "" {
		return explicit
	}
	return ".env"
}

// readDotEnv parses the same repo-root .env the Node simulator reads, so both
// halves of the webhook contract are configured from one file. A missing file
// is not an error.
//
// It returns the values rather than calling os.Setenv, which is what it used
// to do: that left the first Load's file in the process environment forever,
// so a second Load - in a test, or after a working directory change - saw
// values no file on disk still held.
func readDotEnv(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	values := make(map[string]string)
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
		// First occurrence wins, which is what the os.Setenv pass did by never
		// overwriting a key that already existed.
		if _, seen := values[key]; !seen {
			values[key] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}
