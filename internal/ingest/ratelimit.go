package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// tokenBucket is a Lua script because the refill and the take have to happen
// together. Done as GET, compute, SET from Go, two requests arriving at once
// both read the same token count and both spend it.
//
// KEYS[1] bucket
// ARGV[1] tokens per second, ARGV[2] burst, ARGV[3] now (unix millis)
// returns {allowed, tokens_remaining}
var tokenBucket = redis.NewScript(`
local key      = KEYS[1]
local rate     = tonumber(ARGV[1])
local burst    = tonumber(ARGV[2])
local now_ms   = tonumber(ARGV[3])

local state    = redis.call('HMGET', key, 'tokens', 'updated_ms')
local tokens   = tonumber(state[1])
local updated  = tonumber(state[2])

if tokens == nil then
  tokens  = burst
  updated = now_ms
end

-- Refill for the time that has passed, capped at the burst size.
local elapsed = math.max(0, now_ms - updated) / 1000.0
tokens = math.min(burst, tokens + elapsed * rate)

local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end

redis.call('HSET', key, 'tokens', tokens, 'updated_ms', now_ms)
-- Expire well after a full refill so idle buckets do not accumulate forever.
redis.call('PEXPIRE', key, math.ceil((burst / rate) * 1000) + 60000)

return {allowed, math.floor(tokens)}
`)

// Limiter is a per-key token bucket.
//
// The key matters as much as the numbers. One shared bucket for every caller
// turns a single noisy sender into an outage for everyone; a bucket per
// merchant contains the blast radius to the merchant causing it.
type Limiter struct {
	rdb    *redis.Client
	prefix string
	rate   float64 // tokens per second
	burst  int
}

func NewLimiter(rdb *redis.Client, prefix string, perMinute, burst int) *Limiter {
	return &Limiter{
		rdb:    rdb,
		prefix: prefix,
		rate:   float64(perMinute) / 60.0,
		burst:  burst,
	}
}

// Allow spends one token, reporting how many are left.
func (l *Limiter) Allow(ctx context.Context, key string) (bool, int, error) {
	result, err := tokenBucket.Run(
		ctx, l.rdb,
		[]string{l.prefix + ":" + key},
		l.rate, l.burst, time.Now().UnixMilli(),
	).Int64Slice()
	if err != nil {
		return false, 0, fmt.Errorf("rate limit %s: %w", key, err)
	}
	if len(result) != 2 {
		return false, 0, fmt.Errorf("rate limit %s: unexpected script result %v", key, result)
	}
	return result[0] == 1, int(result[1]), nil
}
