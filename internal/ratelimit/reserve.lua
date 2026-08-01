-- KEYS[1..3]  = org, team, key bucket hash keys
-- ARGV[1..3]  = org, team, key capacities (tokens per minute)
-- ARGV[4]     = cost (tokens to reserve)
-- ARGV[5]     = bucket TTL in seconds
--
-- All three scopes are checked for capacity before any is deducted --
-- partial deduction is a bug, not a feature. Evaluated outermost-first
-- (org, then team, then key), matching the README's stated hierarchy, so
-- the first insufficient scope is the one reported.
--
-- Returns {allowed, retry_after_ms, limiting_scope, remaining_key_tokens}.
-- limiting_scope is "" on success.

local t = redis.call('TIME')
local now = tonumber(t[1]) + tonumber(t[2]) / 1000000

local caps = {tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3])}
local cost = tonumber(ARGV[4])
local ttl = tonumber(ARGV[5])
local scopes = {'org', 'team', 'key'}
local balances = {}

for i = 1, 3 do
	local cap = caps[i]
	local rate = cap / 60.0
	local data = redis.call('HMGET', KEYS[i], 'tokens', 'last_refill')

	local tokens
	if data[1] == false then
		-- A missing key means a full, untouched bucket.
		tokens = cap
	else
		local lastRefill = tonumber(data[2])
		local elapsed = now - lastRefill
		if elapsed < 0 then
			elapsed = 0
		end
		tokens = math.min(cap, tonumber(data[1]) + elapsed * rate)
	end
	balances[i] = tokens

	if tokens < cost then
		local deficit = cost - tokens
		local retryAfterMs = math.ceil((deficit / rate) * 1000)
		return { 0, retryAfterMs, scopes[i], 0 }
	end
end

for i = 1, 3 do
	local newTokens = balances[i] - cost
	redis.call('HSET', KEYS[i], 'tokens', newTokens, 'last_refill', now)
	redis.call('EXPIRE', KEYS[i], ttl)
	balances[i] = newTokens
end

return { 1, 0, '', math.floor(balances[3]) }
