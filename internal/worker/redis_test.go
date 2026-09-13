//go:build integration

package worker

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// testRedis is why this file carries the integration tag.
//
// FLUSHDB wipes whatever database 15 holds, not only the keys these tests
// wrote, and an env-var gate is not enough for that: REDIS_URL is set in every
// developer's .env, so `make go-test` would flush on every run. A live test
// that only reads costs nothing and stays gated; one that destroys state it did
// not create is asked for by name, through `make go-test-integration`.
func testRedis(t *testing.T) *redis.Client {
	t.Helper()

	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set; skipping Redis tests")
	}

	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse REDIS_URL: %v", err)
	}
	// A dedicated database, so a test never touches the running system's keys.
	opts.DB = 15

	rdb := redis.NewClient(opts)
	if err := rdb.Ping(t.Context()).Err(); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}

	if err := rdb.FlushDB(t.Context()).Err(); err != nil {
		t.Fatalf("flush test db: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	return rdb
}

func TestClaimReturnsOnlyWhatIsDue(t *testing.T) {
	ctx := t.Context()
	d := NewDeadlines(testRedis(t))
	now := time.Now()

	if err := d.ScheduleMany(ctx, map[int64]time.Time{
		1: now.Add(-time.Hour),   // overdue
		2: now.Add(-time.Minute), // overdue
		3: now.Add(time.Hour),    // not yet
		4: now.Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("ScheduleMany: %v", err)
	}

	ids, err := d.Claim(ctx, now, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("claimed %v, want the two overdue ids", ids)
	}

	// Claiming removes: a second call must not hand out the same work.
	again, err := d.Claim(ctx, now, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second claim returned %v, want nothing", again)
	}

	pending, err := d.Pending(ctx)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending != 2 {
		t.Errorf("pending = %d, want the 2 future disputes still indexed", pending)
	}
}

// The reason claiming is a Lua script. Read-then-remove from Go is two round
// trips, and concurrent workers both read the same ids in the gap.
func TestConcurrentClaimsNeverOverlap(t *testing.T) {
	ctx := t.Context()
	d := NewDeadlines(testRedis(t))
	now := time.Now()

	const total = 500
	entries := make(map[int64]time.Time, total)
	for i := int64(1); i <= total; i++ {
		entries[i] = now.Add(-time.Minute)
	}
	if err := d.ScheduleMany(ctx, entries); err != nil {
		t.Fatalf("ScheduleMany: %v", err)
	}

	var (
		mu   sync.Mutex
		seen = map[int64]int{}
		wg   sync.WaitGroup
	)

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				ids, err := d.Claim(ctx, now, 10)
				if err != nil || len(ids) == 0 {
					return
				}
				mu.Lock()
				for _, id := range ids {
					seen[id]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != total {
		t.Errorf("claimed %d distinct ids, want %d", len(seen), total)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("dispute %d was claimed %d times; it must be handed to exactly one worker", id, count)
		}
	}
}

// ZADD updates the score of a member already present, so rescheduling is not a
// duplicate.
func TestScheduleTwiceMovesRatherThanDuplicates(t *testing.T) {
	ctx := t.Context()
	d := NewDeadlines(testRedis(t))
	now := time.Now()

	if err := d.Schedule(ctx, 42, now.Add(time.Hour)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if err := d.Schedule(ctx, 42, now.Add(-time.Minute)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	pending, _ := d.Pending(ctx)
	if pending != 1 {
		t.Fatalf("pending = %d, want 1", pending)
	}

	ids, err := d.Claim(ctx, now, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(ids) != 1 || ids[0] != 42 {
		t.Errorf("claimed %v, want [42] at its new earlier score", ids)
	}
}

func TestLockIsExclusive(t *testing.T) {
	ctx := t.Context()
	locks := NewLocks(testRedis(t), 5*time.Second)

	lock, err := locks.Acquire(ctx, 7)
	if err != nil || lock == nil {
		t.Fatalf("first Acquire: lock=%v err=%v", lock, err)
	}

	if again, err := locks.Acquire(ctx, 7); err != nil || again != nil {
		t.Errorf("second Acquire: lock=%v err=%v, want nil lock", again, err)
	}

	held, err := lock.Release(ctx)
	if err != nil || !held {
		t.Fatalf("Release: held=%v err=%v", held, err)
	}

	if after, err := locks.Acquire(ctx, 7); err != nil || after == nil {
		t.Errorf("Acquire after release: lock=%v err=%v, want a lock", after, err)
	}
}

// The bug the release script exists to prevent: a holder stalls past its TTL,
// another worker legitimately takes the lock, and the first one wakes up and
// deletes a lock it no longer owns.
func TestReleaseCannotDeleteSomebodyElsesLock(t *testing.T) {
	ctx := t.Context()
	rdb := testRedis(t)
	locks := NewLocks(rdb, 100*time.Millisecond)

	stale, err := locks.Acquire(ctx, 9)
	if err != nil || stale == nil {
		t.Fatalf("Acquire: lock=%v err=%v", stale, err)
	}

	// Let the TTL lapse, then let a second worker take it.
	time.Sleep(200 * time.Millisecond)

	fresh, err := locks.Acquire(ctx, 9)
	if err != nil || fresh == nil {
		t.Fatalf("second Acquire after expiry: lock=%v err=%v", fresh, err)
	}

	// The first worker now tries to release. It must not succeed.
	held, err := stale.Release(ctx)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if held {
		t.Error("the expired holder deleted the new holder's lock")
	}

	// And the real holder is still holding it.
	if other, _ := locks.Acquire(ctx, 9); other != nil {
		t.Error("the lock was released by the wrong holder")
	}
	if held, _ := fresh.Release(ctx); !held {
		t.Error("the real holder could not release its own lock")
	}
}
