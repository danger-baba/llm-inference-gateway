-- KEYS[1..3] = org, team, key bucket hash keys
-- ARGV[1..3] = org, team, key capacities (tokens per minute)
-- ARGV[4]    = delta (positive = refund an over-reservation, negative =
--              charge the difference when actual usage exceeded the
--              estimate)
-- ARGV[5]    = bucket TTL in seconds
--
-- Reconciliation never rejects: a refund is clamped at capacity, and a
-- charge is allowed to drive a bucket negative (the response has already
-- been served; overdrawing just means the next request waits longer).

local t = redis.call('TIME')
local now = tonumber(t[1]) + tonumber(t[2]) / 1000000

local caps = {tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3])}
local delta = tonumber(ARGV[4])
local ttl = tonumber(ARGV[5])

for i = 1, 3 do
	local cap = caps[i]
	local rate = cap / 60.0
	local data = redis.call('HMGET', KEYS[i], 'tokens', 'last_refill')

	local tokens
	if data[1] == false then
		tokens = cap
	else
		local lastRefill = tonumber(data[2])
		local elapsed = now - lastRefill
		if elapsed < 0 then
			elapsed = 0
		end
		tokens = math.min(cap, tonumber(data[1]) + elapsed * rate)
	end

	tokens = tokens + delta
	if tokens > cap then
		tokens = cap
	end

	redis.call('HSET', KEYS[i], 'tokens', tokens, 'last_refill', now)
	redis.call('EXPIRE', KEYS[i], ttl)
end

return 1
