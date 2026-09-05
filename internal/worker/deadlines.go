package worker

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// DeadlineKey is the sorted set of open disputes, scored by their deadline as
// a unix timestamp.
const DeadlineKey = "disputes:deadlines"

// claimDue pops the ids that are due and removes them in the same breath.
//
// ZRANGEBYSCORE followed by ZREM from Go is two round trips with a gap between
// them, and two workers polling at once both read the same ids and both do the
// work. Inside a script it is one atomic operation, so an id is handed to
// exactly one caller.
var claimDue = redis.NewScript(`
local key   = KEYS[1]
local upto  = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])

local ids = redis.call('ZRANGEBYSCORE', key, '-inf', upto, 'LIMIT', 0, limit)
if #ids > 0 then
  redis.call('ZREM', key, unpack(ids))
end
return ids
`)

// Deadlines is the worker's view of what is due when.
//
// It is an index, not a record. Every id in it also exists in Postgres with the
// same deadline, and Reconcile rebuilds the whole set from there - so flushing
// Redis costs a reconcile pass and nothing else. Anything that would be *lost*
// by losing this key does not belong in it.
type Deadlines struct {
	rdb *redis.Client
}

func NewDeadlines(rdb *redis.Client) *Deadlines {
	return &Deadlines{rdb: rdb}
}

// Schedule adds or moves a dispute's entry. ZADD updates the score of a member
// that is already present, so scheduling the same dispute twice is not a
// duplicate - it is a reschedule.
func (d *Deadlines) Schedule(ctx context.Context, disputeID int64, at time.Time) error {
	err := d.rdb.ZAdd(ctx, DeadlineKey, redis.Z{
		Score:  float64(at.Unix()),
		Member: strconv.FormatInt(disputeID, 10),
	}).Err()
	if err != nil {
		return fmt.Errorf("schedule dispute %d: %w", disputeID, err)
	}
	return nil
}

// ScheduleMany is the bulk form used by the reconcile pass.
func (d *Deadlines) ScheduleMany(ctx context.Context, entries map[int64]time.Time) error {
	if len(entries) == 0 {
		return nil
	}

	members := make([]redis.Z, 0, len(entries))
	for id, at := range entries {
		members = append(members, redis.Z{
			Score:  float64(at.Unix()),
			Member: strconv.FormatInt(id, 10),
		})
	}

	if err := d.rdb.ZAdd(ctx, DeadlineKey, members...).Err(); err != nil {
		return fmt.Errorf("schedule %d disputes: %w", len(entries), err)
	}
	return nil
}

// Claim takes up to limit disputes that are due at or before `upto`.
//
// The lookahead matters: claiming only what is already overdue guarantees every
// dispute is handled late. Work is claimed slightly *before* it is due so the
// decision lands inside the window.
func (d *Deadlines) Claim(ctx context.Context, upto time.Time, limit int) ([]int64, error) {
	raw, err := claimDue.Run(ctx, d.rdb, []string{DeadlineKey}, upto.Unix(), limit).StringSlice()
	if err != nil {
		return nil, fmt.Errorf("claim due disputes: %w", err)
	}

	ids := make([]int64, 0, len(raw))
	for _, member := range raw {
		id, parseErr := strconv.ParseInt(member, 10, 64)
		if parseErr != nil {
			// A member that is not an id cannot be acted on and would be
			// claimed forever. It has already been removed by the script.
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// Drop removes a dispute that no longer needs watching.
func (d *Deadlines) Drop(ctx context.Context, disputeID int64) error {
	return d.rdb.ZRem(ctx, DeadlineKey, strconv.FormatInt(disputeID, 10)).Err()
}

// Pending is how many disputes are currently indexed, for the log line that
// tells you whether a reconcile did anything.
func (d *Deadlines) Pending(ctx context.Context) (int64, error) {
	return d.rdb.ZCard(ctx, DeadlineKey).Result()
}
