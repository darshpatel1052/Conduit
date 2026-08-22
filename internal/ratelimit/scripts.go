package ratelimit

import "github.com/redis/go-redis/v9"

// The limiters below run as Lua because the check-then-increment has to be
// atomic. Doing it as GET followed by INCR from Go opens a race: two gateway
// replicas both read 59/60, both decide there is room, and both increment —
// the limit is exceeded and nobody noticed. Redis executes a script as a single
// unit, so the decision and the write cannot interleave with another replica's.
//
// Everything is computed inside the script for a second reason too: it is one
// network round trip. The limiter sits in front of every request, so a
// two-round-trip design would add Redis latency to the whole gateway twice.

// slidingWindowScript implements a sliding window counter.
//
// A fixed window is simpler but has a well-known flaw: with a limit of 60/min,
// a client can send 60 requests at 11:59:59 and 60 more at 12:00:00 — 120 in
// one second, entirely within the rules. The sliding window fixes this by
// weighting the previous window's count by how much of it is still in view:
//
//	estimate = prev_count * (1 - elapsed_in_current/window) + current_count
//
// That approximates a true rolling count while storing just two integers per
// key, instead of the sorted set of timestamps an exact implementation needs.
// The approximation assumes the previous window's requests were spread evenly;
// the error is bounded and always conservative enough for a safety valve.
//
// KEYS[1] base key
// ARGV[1] now (unix ms)
// ARGV[2] window (ms)
// ARGV[3] limit
// ARGV[4] cost
// ARGV[5] dry_run: 1 = report only, do not consume
// ARGV[6] force:   1 = record the cost even when it exceeds the limit
//
// Returns {allowed, remaining, retry_after_ms, used}
var slidingWindowScript = redis.NewScript(`
local now    = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit  = tonumber(ARGV[3])
local cost   = tonumber(ARGV[4])
local dry    = tonumber(ARGV[5])
local force  = tonumber(ARGV[6])

if limit <= 0 then
  return {1, -1, 0, 0}
end

local current_index = math.floor(now / window)
local elapsed       = now - (current_index * window)
local weight        = 1.0 - (elapsed / window)

local cur_key  = KEYS[1] .. ':' .. current_index
local prev_key = KEYS[1] .. ':' .. (current_index - 1)

local cur  = tonumber(redis.call('GET', cur_key)  or '0')
local prev = tonumber(redis.call('GET', prev_key) or '0')

local used = (prev * weight) + cur
local over = (used + cost) > limit

-- A gate refuses and records nothing, because the request does not happen. A
-- debit has no such option: the cost was already incurred, and declining to
-- record it does not un-spend it — it just loses the number. Without force,
-- any single request costing more than the whole budget would be recorded as
-- costing nothing, making the most expensive requests the only free ones.
if over and force == 0 then
  -- Time until enough of the previous window ages out to admit this request.
  local retry = window - elapsed
  return {0, 0, math.ceil(retry), math.floor(used + 0.5)}
end

if dry == 0 then
  redis.call('INCRBY', cur_key, cost)
  -- Two windows of TTL: the current bucket must outlive its own window because
  -- the *next* window still reads it as the previous one.
  redis.call('PEXPIRE', cur_key, window * 2)
end

local remaining = limit - (used + cost)
if remaining < 0 then remaining = 0 end

local allowed = 1
if over then allowed = 0 end

return {allowed, math.floor(remaining), 0, math.floor(used + cost + 0.5)}
`)

// tokenBucketScript implements a token bucket.
//
// Where the sliding window smooths traffic, a bucket deliberately permits a
// burst: it refills at a steady rate up to a capacity, so an idle client can
// spend accumulated allowance all at once. That suits interactive traffic — a
// user pasting three prompts in a row should not be throttled — while still
// bounding the long-run rate.
//
// State is two fields (tokens, last-refill timestamp) rather than a counter
// per window, and refill is computed lazily on read, so an idle key costs
// nothing until it is touched again.
//
// KEYS[1] key
// ARGV[1] now (unix ms)
// ARGV[2] window (ms)   — the period over which `limit` tokens are granted
// ARGV[3] limit         — tokens granted per window (the steady rate)
// ARGV[4] burst         — bucket capacity
// ARGV[5] cost
// ARGV[6] dry_run
// ARGV[7] force: 1 = deduct even when the bucket cannot cover the cost
//
// Returns {allowed, remaining, retry_after_ms, used}
var tokenBucketScript = redis.NewScript(`
local now    = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit  = tonumber(ARGV[3])
local burst  = tonumber(ARGV[4])
local cost   = tonumber(ARGV[5])
local dry    = tonumber(ARGV[6])
local force  = tonumber(ARGV[7])

if limit <= 0 then
  return {1, -1, 0, 0}
end
if burst <= 0 then burst = limit end

local state  = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(state[1])
local ts     = tonumber(state[2])

if tokens == nil then
  -- A new client starts with a full bucket rather than empty: penalising a
  -- first-time caller for having no history would be backwards.
  tokens = burst
  ts = now
end

-- Lazy refill for the time elapsed since the bucket was last touched.
local rate    = limit / window            -- tokens per millisecond
local elapsed = math.max(0, now - ts)
tokens = math.min(burst, tokens + (elapsed * rate))

if tokens < cost and force == 0 then
  local deficit = cost - tokens
  local retry   = math.ceil(deficit / rate)
  -- Persist the refill so the next call does not recompute from a stale ts.
  if dry == 0 then
    redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
    redis.call('PEXPIRE', KEYS[1], math.ceil(window * 2))
  end
  return {0, math.floor(tokens), retry, math.floor(burst - tokens + 0.5)}
end

-- Forced deduction is allowed to drive the bucket negative. That is what
-- carries an overage: the balance refills from below zero, so the time until
-- the next request is admitted grows with how far over budget the last one
-- went, rather than being forgiven at the window edge.
local over = tokens < cost

if dry == 0 then
  tokens = tokens - cost
  redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
  -- Expire after the time it would take to refill completely; past that point
  -- the state is indistinguishable from a fresh full bucket. A negative
  -- balance takes longer to climb back, so the deficit is included.
  local refill = (burst - math.min(0, tokens)) / rate
  redis.call('PEXPIRE', KEYS[1], math.ceil(refill + window))
end

local allowed = 1
if over then allowed = 0 end
local remaining = math.floor(tokens)
if remaining < 0 then remaining = 0 end

return {allowed, remaining, 0, math.floor(burst - tokens + 0.5)}
`)
