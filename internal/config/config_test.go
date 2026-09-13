package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// loadWithout points Load at a .env that does not exist, so only the process
// environment (and t.Setenv) decides what it sees.
func loadWithout(t *testing.T) (Config, error) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("DOTENV_PATH", filepath.Join(t.TempDir(), ".env"))
	return Load()
}

func TestUnparseableValueIsAnError(t *testing.T) {
	cases := []struct {
		name, key, value string
	}{
		{"duration without a unit", "WORKER_LOCK_TTL", "30"},
		{"integer that is not one", "WORKER_CONCURRENCY", "eight"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(c.key, c.value)
			_, err := loadWithout(t)
			if err == nil {
				t.Fatalf("%s=%q: Load accepted it", c.key, c.value)
			}
			if !strings.Contains(err.Error(), c.key) {
				t.Fatalf("error does not name %s: %v", c.key, err)
			}
		})
	}
}

// Every bad variable is reported in one go, so the operator fixes them all in
// one restart instead of discovering them one at a time.
func TestAllParseFailuresAreReported(t *testing.T) {
	t.Setenv("WORKER_LOCK_TTL", "30")
	t.Setenv("WORKER_CONCURRENCY", "eight")
	_, err := loadWithout(t)
	if err == nil {
		t.Fatal("Load accepted two unparseable values")
	}
	for _, key := range []string{"WORKER_LOCK_TTL", "WORKER_CONCURRENCY"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not name %s: %v", key, err)
		}
	}
}

func TestAbsentOrEmptyValueUsesFallback(t *testing.T) {
	t.Setenv("WORKER_LOCK_TTL", "")
	t.Setenv("WORKER_CONCURRENCY", "")
	cfg, err := loadWithout(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Worker.LockTTL != 30*time.Second {
		t.Errorf("Worker.LockTTL = %v, want the 30s fallback", cfg.Worker.LockTTL)
	}
	if cfg.Worker.Concurrency != 8 {
		t.Errorf("Worker.Concurrency = %d, want the fallback 8", cfg.Worker.Concurrency)
	}
}

func TestParsedValueWins(t *testing.T) {
	t.Setenv("WORKER_LOCK_TTL", "45s")
	t.Setenv("WORKER_CONCURRENCY", "3")
	cfg, err := loadWithout(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Worker.LockTTL != 45*time.Second || cfg.Worker.Concurrency != 3 {
		t.Errorf("got lock ttl %v, concurrency %d", cfg.Worker.LockTTL, cfg.Worker.Concurrency)
	}
}

// Load reads .env; it does not install it. Two Loads pointed at two files see
// two sets of values, which is only true because nothing calls os.Setenv - the
// old behaviour left the first file's values in the process environment for
// the rest of its life, so the second Load returned the first one's answer.
func TestLoadDoesNotChangeTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	load := func(contents string) Config {
		t.Helper()
		path := filepath.Join(dir, ".env")
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("write .env: %v", err)
		}
		t.Setenv("DOTENV_PATH", path)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return cfg
	}

	first := load("WORKER_LOCK_TTL=45s\n")
	if first.Worker.LockTTL != 45*time.Second {
		t.Fatalf("first load: lock ttl %v, want 45s from the file", first.Worker.LockTTL)
	}
	if _, leaked := os.LookupEnv("WORKER_LOCK_TTL"); leaked {
		t.Error("Load exported WORKER_LOCK_TTL into the process environment")
	}

	second := load("WORKER_LOCK_TTL=90s\nWORKER_POLL_INTERVAL=9s\n")
	if second.Worker.LockTTL != 90*time.Second {
		t.Errorf("second load: lock ttl %v, want 90s from the second file", second.Worker.LockTTL)
	}

	// And the precedence the file never had a chance to state before, since
	// os.Setenv only ever wrote variables the environment did not already hold:
	// the process environment wins over .env.
	t.Setenv("WORKER_POLL_INTERVAL", "3s")
	third := load("WORKER_LOCK_TTL=90s\nWORKER_POLL_INTERVAL=9s\n")
	if third.Worker.PollInterval != 3*time.Second {
		t.Errorf("poll interval %v, want the environment's 3s over the file's 9s", third.Worker.PollInterval)
	}
}

// Load itself no longer refuses a missing database, so cmd/dlq - which moves
// messages between two SQS queues and opens no pool - starts without one. The
// nine binaries that do open a pool ask for it.
func TestTheDatabaseRequirementBelongsToTheBinary(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DOTENV_PATH", filepath.Join(t.TempDir(), ".env"))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with no DATABASE_URL: %v, want it to load", err)
	}
	err = cfg.RequireDatabase()
	if err == nil {
		t.Fatal("RequireDatabase accepted an empty DATABASE_URL")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error does not name DATABASE_URL: %v", err)
	}

	cfg.DatabaseURL = "postgres://localhost/test"
	if err := cfg.RequireDatabase(); err != nil {
		t.Errorf("RequireDatabase with a database set: %v", err)
	}
}
