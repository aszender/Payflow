package middleware

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	idempotencyInProgressTTL = 60 * time.Second
	idempotencyCompletedTTL  = 24 * time.Hour
)

//go:embed idempotency_check.lua
var idempotencyCheckLua string

type IdempotencyDecision string

const (
	IdempotencyProceed    IdempotencyDecision = "PROCEED"
	IdempotencyInProgress IdempotencyDecision = "IN_PROGRESS"
	IdempotencyCompleted  IdempotencyDecision = "COMPLETED"
)

type IdempotencyResult struct {
	Decision   IdempotencyDecision
	StatusCode int
	Body       []byte
}

type IdempotencyStore struct {
	client *redis.Client
	script *redis.Script
}

func NewIdempotencyStore(client *redis.Client) *IdempotencyStore {
	return &IdempotencyStore{
		client: client,
		script: redis.NewScript(idempotencyCheckLua),
	}
}

func (s *IdempotencyStore) Check(ctx context.Context, merchantID, key string) (IdempotencyResult, error) {
	if s == nil || s.client == nil || merchantID == "" || key == "" {
		return IdempotencyResult{Decision: IdempotencyProceed}, nil
	}

	values, err := s.script.Run(ctx, s.client, []string{idempotencyRedisKey(merchantID, key)}).Slice()
	if err != nil {
		return IdempotencyResult{}, err
	}
	if len(values) == 0 {
		return IdempotencyResult{Decision: IdempotencyProceed}, nil
	}

	decision := IdempotencyDecision(fmt.Sprint(values[0]))
	result := IdempotencyResult{Decision: decision}
	if decision != IdempotencyCompleted {
		return result, nil
	}

	if len(values) > 1 {
		status, err := strconv.Atoi(fmt.Sprint(values[1]))
		if err == nil {
			result.StatusCode = status
		}
	}
	if len(values) > 2 {
		result.Body = []byte(fmt.Sprint(values[2]))
	}
	return result, nil
}

func (s *IdempotencyStore) StoreCompleted(ctx context.Context, merchantID, key string, statusCode int, body []byte) error {
	if s == nil || s.client == nil || merchantID == "" || key == "" {
		return nil
	}

	pipe := s.client.TxPipeline()
	pipe.HSet(ctx, idempotencyRedisKey(merchantID, key),
		"state", "completed",
		"status_code", statusCode,
		"response_body", string(body),
	)
	pipe.Expire(ctx, idempotencyRedisKey(merchantID, key), idempotencyCompletedTTL)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *IdempotencyStore) Release(ctx context.Context, merchantID, key string) error {
	if s == nil || s.client == nil || merchantID == "" || key == "" {
		return nil
	}
	return s.client.Del(ctx, idempotencyRedisKey(merchantID, key)).Err()
}

func idempotencyRedisKey(merchantID, key string) string {
	return fmt.Sprintf("idempotency:{%s}:%s", merchantID, key)
}
