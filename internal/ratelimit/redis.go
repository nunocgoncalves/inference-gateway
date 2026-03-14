package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	rpmKeyPrefix = "ratelimit:rpm:"
	tpmKeyPrefix = "ratelimit:tpm:"
	windowSize   = 60 * time.Second
)

// RedisLimiter implements Limiter using Redis sorted sets for sliding window
// rate limiting. Each member in the sorted set has score = timestamp (unix
// microseconds) and value = a unique ID. The count of members within the
// window determines current usage.
type RedisLimiter struct {
	rdb *redis.Client
}

// NewRedisLimiter creates a new Redis-backed rate limiter.
func NewRedisLimiter(rdb *redis.Client) *RedisLimiter {
	return &RedisLimiter{rdb: rdb}
}

// CheckRPM atomically checks and increments the RPM counter using a Lua script.
// This ensures the check-and-increment is a single atomic operation.
func (l *RedisLimiter) CheckRPM(ctx context.Context, keyID string, limit int) (*Result, error) {
	key := rpmKeyPrefix + keyID
	return l.checkAndIncrement(ctx, key, limit, 1)
}

// CheckTPM checks the current TPM usage without incrementing. This is called
// before forwarding a request to verify the key hasn't exceeded its token budget.
func (l *RedisLimiter) CheckTPM(ctx context.Context, keyID string, limit int) (*Result, error) {
	key := tpmKeyPrefix + keyID
	return l.check(ctx, key, limit)
}

// IncrementTPM adds tokens to the TPM sliding window. Called after receiving
// token counts from the response (or periodically during streaming).
func (l *RedisLimiter) IncrementTPM(ctx context.Context, keyID string, tokens int) error {
	if tokens <= 0 {
		return nil
	}
	key := tpmKeyPrefix + keyID
	_, err := l.checkAndIncrement(ctx, key, 0, tokens)
	return err
}

// slidingWindowScript is a Lua script that atomically:
// 1. Removes expired entries outside the window
// 2. Counts current entries
// 3. If under limit, adds `count` new entries
// 4. Sets the key TTL
//
// KEYS[1] = the sorted set key
// ARGV[1] = window start (unix microseconds)
// ARGV[2] = now (unix microseconds)
// ARGV[3] = limit (0 = no limit check, just increment)
// ARGV[4] = count to add (number of entries/tokens to insert)
// ARGV[5] = TTL in seconds
//
// Returns: {current_count, allowed (0 or 1)}
var slidingWindowScript = redis.NewScript(`
local key = KEYS[1]
local window_start = tonumber(ARGV[1])
local now = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local count = tonumber(ARGV[4])
local ttl = tonumber(ARGV[5])

-- Remove expired entries.
redis.call('ZREMRANGEBYSCORE', key, '-inf', window_start)

-- Count current entries (sum of all member values for TPM, count for RPM).
local current = 0
local members = redis.call('ZRANGEBYSCORE', key, window_start, '+inf', 'WITHSCORES')
for i = 1, #members, 2 do
    local member = members[i]
    -- Member format: "count:timestamp:random" — extract count.
    local member_count = tonumber(string.match(member, '^(%d+):'))
    if member_count then
        current = current + member_count
    else
        current = current + 1
    end
end

-- Check limit (0 means skip limit check).
local allowed = 1
if limit > 0 and current + count > limit then
    allowed = 0
else
    -- Add new entry: "count:now:random" to keep members unique.
    local member = count .. ':' .. now .. ':' .. math.random(1000000)
    redis.call('ZADD', key, now, member)
    redis.call('EXPIRE', key, ttl)
end

return {current, allowed}
`)

func (l *RedisLimiter) checkAndIncrement(ctx context.Context, key string, limit int, count int) (*Result, error) {
	now := time.Now()
	nowMicro := now.UnixMicro()
	windowStart := now.Add(-windowSize).UnixMicro()
	ttl := int(windowSize.Seconds()) + 1

	vals, err := slidingWindowScript.Run(ctx, l.rdb, []string{key},
		windowStart, nowMicro, limit, count, ttl,
	).Int64Slice()
	if err != nil {
		return nil, fmt.Errorf("running rate limit script: %w", err)
	}

	current := int(vals[0])
	allowed := vals[1] == 1

	remaining := 0
	if limit > 0 {
		remaining = limit - current
		if allowed {
			remaining -= count
		}
		if remaining < 0 {
			remaining = 0
		}
	}

	return &Result{
		Allowed:   allowed,
		Limit:     limit,
		Remaining: remaining,
		ResetAt:   now.Add(windowSize),
	}, nil
}

func (l *RedisLimiter) check(ctx context.Context, key string, limit int) (*Result, error) {
	now := time.Now()
	windowStart := now.Add(-windowSize).UnixMicro()

	// Get all members in the window.
	members, err := l.rdb.ZRangeByScore(ctx, key, &redis.ZRangeBy{
		Min: fmt.Sprintf("%d", windowStart),
		Max: "+inf",
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("checking rate limit: %w", err)
	}

	// Sum up the counts from member names.
	current := 0
	for _, member := range members {
		var count int
		if _, err := fmt.Sscanf(member, "%d:", &count); err == nil {
			current += count
		} else {
			current++
		}
	}

	remaining := limit - current
	if remaining < 0 {
		remaining = 0
	}

	return &Result{
		Allowed:   current < limit,
		Limit:     limit,
		Remaining: remaining,
		ResetAt:   now.Add(windowSize),
	}, nil
}

// TPMBatcher accumulates token counts and flushes them to the limiter
// periodically. Used during streaming to avoid a Redis call per SSE chunk.
type TPMBatcher struct {
	limiter  Limiter
	keyID    string
	interval time.Duration

	pending  int
	ticker   *time.Ticker
	stopCh   chan struct{}
	tokensCh chan int
	done     chan struct{}
}

// NewTPMBatcher creates a batcher that flushes accumulated tokens to the
// limiter every `interval`.
func NewTPMBatcher(limiter Limiter, keyID string, interval time.Duration) *TPMBatcher {
	return &TPMBatcher{
		limiter:  limiter,
		keyID:    keyID,
		interval: interval,
		tokensCh: make(chan int, 256),
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start begins the background flush loop.
func (b *TPMBatcher) Start() {
	b.ticker = time.NewTicker(b.interval)
	go b.loop()
}

// Add queues tokens to be flushed on the next interval.
func (b *TPMBatcher) Add(tokens int) {
	select {
	case b.tokensCh <- tokens:
	default:
		// Channel full — accumulate will happen on next flush.
	}
}

// Stop flushes any remaining tokens and stops the background loop.
func (b *TPMBatcher) Stop(ctx context.Context) {
	close(b.stopCh)
	<-b.done

	// Final flush of anything remaining.
	if b.pending > 0 {
		b.limiter.IncrementTPM(ctx, b.keyID, b.pending) //nolint:errcheck
		b.pending = 0
	}
}

func (b *TPMBatcher) loop() {
	defer close(b.done)
	defer b.ticker.Stop()

	for {
		select {
		case <-b.stopCh:
			// Drain the tokens channel.
			for {
				select {
				case tokens := <-b.tokensCh:
					b.pending += tokens
				default:
					return
				}
			}
		case tokens := <-b.tokensCh:
			b.pending += tokens
		case <-b.ticker.C:
			b.flush()
		}
	}
}

func (b *TPMBatcher) flush() {
	// Drain all pending tokens from the channel.
	for {
		select {
		case tokens := <-b.tokensCh:
			b.pending += tokens
		default:
			goto done
		}
	}
done:
	if b.pending > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		b.limiter.IncrementTPM(ctx, b.keyID, b.pending) //nolint:errcheck
		cancel()
		b.pending = 0
	}
}
