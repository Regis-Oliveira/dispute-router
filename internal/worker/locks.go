package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// release deletes the lock only if this caller still holds it.
//
// A bare DEL is the classic distributed-lock bug: the holder stalls, the TTL
// expires, another worker takes the lock, and then the first one wakes up and
// deletes a lock it no longer owns. Comparing the token before deleting has to
// be atomic, which means a script.
var release = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// Locks stops two workers doing the same work at the same time.
//
// It is an optimisation, not the correctness mechanism, and the difference
// matters. Any lock with a timeout can be held by two processes at once - the
// holder pauses for a GC or a slow disk, the TTL lapses, and a second worker
// acquires it perfectly legitimately. What actually makes a double refund
// impossible is downstream: the optimistic version check on the dispute row and
// the unique external_ref on the ledger entry. This just means the second
// worker usually does not bother trying.
type Locks struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewLocks wraps a Redis client; ttl bounds how long a stalled holder keeps
// a lock.
func NewLocks(rdb *redis.Client, ttl time.Duration) *Locks {
	return &Locks{rdb: rdb, ttl: ttl}
}

func lockKey(disputeID int64) string {
	return "lock:dispute:" + strconv.FormatInt(disputeID, 10)
}

// Acquire returns a token, or ok=false if somebody else holds the lock.
func (l *Locks) Acquire(ctx context.Context, disputeID int64) (string, bool, error) {
	token, err := newToken()
	if err != nil {
		return "", false, err
	}

	ok, err := l.rdb.SetNX(ctx, lockKey(disputeID), token, l.ttl).Result()
	if err != nil {
		return "", false, fmt.Errorf("acquire lock for dispute %d: %w", disputeID, err)
	}
	if !ok {
		return "", false, nil
	}
	return token, true, nil
}

// Release gives the lock back. A release that finds someone else's token is not
// an error - it means this worker overran its TTL, which is worth knowing but
// not worth failing over, because the version check already protected the data.
func (l *Locks) Release(ctx context.Context, disputeID int64, token string) (bool, error) {
	deleted, err := release.Run(ctx, l.rdb, []string{lockKey(disputeID)}, token).Int64()
	if err != nil {
		return false, fmt.Errorf("release lock for dispute %d: %w", disputeID, err)
	}
	return deleted == 1, nil
}

func newToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate lock token: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}
