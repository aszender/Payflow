-- Token bucket rate limiter.
-- KEYS[1] = rate_limit:{client_id}
-- ARGV[1] = refill rate per second
-- ARGV[2] = burst size

local key = KEYS[1]
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])

local now_parts = redis.call("TIME")
local now = tonumber(now_parts[1]) + (tonumber(now_parts[2]) / 1000000)

local bucket = redis.call("HMGET", key, "tokens", "last")
local tokens = tonumber(bucket[1])
local last = tonumber(bucket[2])

if not tokens or not last then
  tokens = burst
  last = now
else
  local elapsed = now - last
  if elapsed > 0 then
    tokens = tokens + (elapsed * rate)
    if tokens > burst then
      tokens = burst
    end
  end
  last = now
end

local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end

redis.call("HSET", key, "tokens", tokens, "last", last)
redis.call("EXPIRE", key, 60)

return allowed
