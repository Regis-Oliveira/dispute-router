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
		ShutdownTimeout:      dur("INGEST_SHUTDOWN_TIMEOUT", 15*time.Second),
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.RateLimitBurst <= 0 || cfg.RateLimitPerMinute <= 0 {
		return Config{}, fmt.Errorf("rate limit settings must be positive")
	}
	return cfg, nil
}

func str(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
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
