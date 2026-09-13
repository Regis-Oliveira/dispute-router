package config

import (
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
	if cfg.WorkerLockTTL != 30*time.Second {
		t.Errorf("WorkerLockTTL = %v, want the 30s fallback", cfg.WorkerLockTTL)
	}
	if cfg.WorkerConcurrency != 8 {
		t.Errorf("WorkerConcurrency = %d, want the fallback 8", cfg.WorkerConcurrency)
	}
}

func TestParsedValueWins(t *testing.T) {
	t.Setenv("WORKER_LOCK_TTL", "45s")
	t.Setenv("WORKER_CONCURRENCY", "3")
	cfg, err := loadWithout(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerLockTTL != 45*time.Second || cfg.WorkerConcurrency != 3 {
		t.Errorf("got lock ttl %v, concurrency %d", cfg.WorkerLockTTL, cfg.WorkerConcurrency)
	}
}
