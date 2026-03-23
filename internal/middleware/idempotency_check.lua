-- Idempotency coordination script.
-- KEYS[1] = idempotency:{merchant_id}:{idempotency_key}
-- Returns:
--   PROCEED
--   IN_PROGRESS
--   COMPLETED, status_code, response_body

local key = KEYS[1]

local existing = redis.call("HMGET", key, "state", "status_code", "response_body")
local state = existing[1]

if state then
  if state == "completed" then
    return {"COMPLETED", existing[2] or "", existing[3] or ""}
  end
  if state == "in_progress" then
    return {"IN_PROGRESS"}
  end
end

redis.call("HSET", key, "state", "in_progress")
redis.call("EXPIRE", key, 60)

return {"PROCEED"}
