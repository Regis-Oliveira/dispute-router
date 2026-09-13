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

// Lock is one held dispute lock: the id and the token that proves ownership of
// it, carried together.
//
// Acquire used to return (token, bool, error) and Release used to take the id
// and the token back as separate arguments, which made it the caller's job to
// keep two values paired across the body of a handler. Pairing them here means
// a caller cannot release dispute 7 with dispute 9's token, which is the one
// mistake the release script cannot catch: the token would be the wrong one for
// the key, the script would decline to delete, and the lock would look released
// while it sat there for the rest of its TTL.
type Lock struct {
	locks     *Locks
	disputeID int64
	token     string
}

// Acquire takes the lock for one dispute. A nil Lock with a nil error means
// somebody else holds it, which is an ordinary outcome rather than a failure.
func (l *Locks) Acquire(ctx context.Context, disputeID int64) (*Lock, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}

	ok, err := l.rdb.SetNX(ctx, lockKey(disputeID), token, l.ttl).Result()
	if err != nil {
		return nil, fmt.Errorf("acquire lock for dispute %d: %w", disputeID, err)
	}
	if !ok {
		return nil, nil
	}
	return &Lock{locks: l, disputeID: disputeID, token: token}, nil
}

// Release gives the lock back, reporting whether this holder still had it. A
// release that finds someone else's token is not an error - it means this
// worker overran its TTL, which is worth knowing but not worth failing over,
// because the version check already protected the data.
//
// ctx should outlive the cancellation that ended the work: a release skipped at
// shutdown leaves the lock standing for the whole TTL.
func (lock *Lock) Release(ctx context.Context) (held bool, err error) {
	deleted, err := release.Run(ctx, lock.locks.rdb,
		[]string{lockKey(lock.disputeID)}, lock.token).Int64()
	if err != nil {
		return false, fmt.Errorf("release lock for dispute %d: %w", lock.disputeID, err)
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
