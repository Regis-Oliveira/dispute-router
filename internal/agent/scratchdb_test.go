package agent

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The tests below write agent_runs, and agent_runs cannot be deleted - the
// append-only trigger is doing exactly what it was built for. So they get their
// own database, migrated from the same files the real one uses and dropped
// afterwards.
//
// Running them against the shared development database would mean either
// leaving rows behind on every run or disabling the trigger to clean up, and
// the second would test a table that behaves differently from the one that
// ships.
func scratchDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping the agent store tests")
	}

	name := fmt.Sprintf("agent_scratch_%d", time.Now().UnixNano())
	ctx := t.Context()

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close(ctx)
		t.Fatalf("create scratch database: %v", err)
	}
	admin.Close(ctx)

	pool, err := pgxpool.New(ctx, replaceDBName(t, dsn, name))
	if err != nil {
		t.Fatalf("connect to scratch: %v", err)
	}

	// Simple protocol: a migration file is many statements, and the extended
	// protocol pgx defaults to sends one at a time.
	migrate(t, pool)

	t.Cleanup(func() {
		pool.Close()
		admin, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Logf("scratch database %s left behind: %v", name, err)
			return
		}
		defer admin.Close(context.Background())
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name); err != nil {
			t.Logf("scratch database %s left behind: %v", name, err)
		}
	})
	return pool
}

func replaceDBName(t *testing.T, dsn, name string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	parsed.Path = "/" + name
	return parsed.String()
}

func migrate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	dir := filepath.Join("..", "..", "db", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}

	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if _, err := conn.Conn().PgConn().Exec(t.Context(), string(body)).ReadAll(); err != nil {
			t.Fatalf("apply %s: %v", entry.Name(), err)
		}
	}
}

// fixture puts one merchant, one transaction and one open chargeback in the
// scratch database and returns the dispute id.
func fixture(t *testing.T, pool *pgxpool.Pool, kind string, deadline time.Duration) int64 {
	t.Helper()
	ctx := t.Context()

	var merchantID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO merchants (external_id, name, webhook_secret, currency)
		VALUES ('mrc_test_' || gen_random_uuid(), 'Test Merchant', 'whsec_test', 'USD')
		RETURNING id`).Scan(&merchantID); err != nil {
		t.Fatalf("insert merchant: %v", err)
	}

	var transactionID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO transactions (merchant_id, external_id, amount_minor, currency,
		                          card_bin, card_last4, card_network, customer_ref,
		                          customer_email, descriptor, captured_at)
		VALUES ($1, 'txn_' || gen_random_uuid(), 4100, 'USD', '424242', '4242', 'visa',
		        'cus_test', 'someone@example.test', 'TEST MERCHANT', now() - interval '30 days')
		RETURNING id`, merchantID).Scan(&transactionID); err != nil {
		t.Fatalf("insert transaction: %v", err)
	}

	var disputeID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO disputes (merchant_id, transaction_id, external_id, kind, card_network,
		                      reason_code, amount_minor, currency, deadline_at, opened_at)
		VALUES ($1, $2, 'dsp_' || gen_random_uuid(), $3, 'visa', '10.4', 4100, 'USD',
		        now() + $4::interval, now())
		RETURNING id`, merchantID, transactionID, kind, deadline.String()).Scan(&disputeID); err != nil {
		t.Fatalf("insert dispute: %v", err)
	}
	return disputeID
}

func stateOf(t *testing.T, pool *pgxpool.Pool, disputeID int64) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(t.Context(),
		"SELECT state FROM disputes WHERE id = $1", disputeID).Scan(&state); err != nil {
		t.Fatalf("read state: %v", err)
	}
	return state
}

func runNum(t *testing.T, pool *pgxpool.Pool, disputeID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(),
		"SELECT count(*) FROM agent_runs WHERE dispute_id = $1", disputeID).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	return n
}
