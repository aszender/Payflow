package middleware

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/redis/go-redis/v9"
)

//go:embed rate_limit.lua
var rateLimitLua string

type RedisRateLimiter struct {
	client *redis.Client
	rate   float64
	burst  float64
	script *redis.Script
}

func NewRedisRateLimiter(client *redis.Client, rate float64, burst float64) *RedisRateLimiter {
	return &RedisRateLimiter{
		client: client,
		rate:   rate,
		burst:  burst,
		script: redis.NewScript(rateLimitLua),
	}
}

func (rl *RedisRateLimiter) Allow(ctx context.Context, key string) (bool, error) {
	if rl == nil || rl.client == nil {
		return true, nil
	}

	res, err := rl.script.Run(ctx, rl.client, []string{rateLimitRedisKey(key)}, rl.rate, rl.burst).Int()
	if err != nil {
		return true, nil
	}

	return res == 1, nil
}

func rateLimitRedisKey(clientID string) string {
	return fmt.Sprintf("rate_limit:{%s}", clientID)
}
