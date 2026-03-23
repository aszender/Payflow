package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRedisRateLimiter_AllowsBurstThenRejects(t *testing.T) {
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	limiter := NewRedisRateLimiter(client, 1, 2)
	ctx := context.Background()

	for range 2 {
		allowed, err := limiter.Allow(ctx, "client-1")
		if err != nil {
			t.Fatalf("allow returned error: %v", err)
		}
		if !allowed {
			t.Fatal("expected request to be allowed within burst")
		}
	}

	allowed, err := limiter.Allow(ctx, "client-1")
	if err != nil {
		t.Fatalf("allow returned error: %v", err)
	}
	if allowed {
		t.Fatal("expected request after burst to be rejected")
	}
}

func TestRedisRateLimiter_RefillsOverTime(t *testing.T) {
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	limiter := NewRedisRateLimiter(client, 1, 1)
	ctx := context.Background()

	allowed, _ := limiter.Allow(ctx, "client-2")
	if !allowed {
		t.Fatal("expected initial request to be allowed")
	}

	allowed, _ = limiter.Allow(ctx, "client-2")
	if allowed {
		t.Fatal("expected second request to be rejected before refill")
	}

	now, err := client.Time(ctx).Result()
	if err != nil {
		t.Fatalf("redis time: %v", err)
	}
	last := float64(now.Add(-2*time.Second).UnixNano()) / float64(time.Second)
	if err := client.HSet(ctx, rateLimitRedisKey("client-2"), "tokens", 0, "last", fmt.Sprintf("%.6f", last)).Err(); err != nil {
		t.Fatalf("hset bucket: %v", err)
	}

	allowed, _ = limiter.Allow(ctx, "client-2")
	if !allowed {
		t.Fatal("expected request after refill to be allowed")
	}
}

func TestRedisRateLimiter_IsSafeUnderConcurrency(t *testing.T) {
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	limiter := NewRedisRateLimiter(client, 0, 5)
	ctx := context.Background()

	var allowed int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := limiter.Allow(ctx, "client-3")
			if err != nil {
				t.Errorf("allow returned error: %v", err)
				return
			}
			if ok {
				atomic.AddInt32(&allowed, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&allowed); got != 5 {
		t.Fatalf("expected exactly 5 allowed requests, got %d", got)
	}
}

func TestRedisRateLimiter_FailOpenWhenRedisUnavailable(t *testing.T) {
	client := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  10 * time.Millisecond,
		ReadTimeout:  10 * time.Millisecond,
		WriteTimeout: 10 * time.Millisecond,
	})
	t.Cleanup(func() { _ = client.Close() })

	limiter := NewRedisRateLimiter(client, 1, 1)
	allowed, err := limiter.Allow(context.Background(), "client-down")
	if err != nil {
		t.Fatalf("expected fail-open without error, got %v", err)
	}
	if !allowed {
		t.Fatal("expected request to be allowed when Redis is unavailable")
	}
}

func TestRedisRateLimiter_TTLExpirationResetsBucket(t *testing.T) {
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	limiter := NewRedisRateLimiter(client, 0, 1)
	ctx := context.Background()

	allowed, _ := limiter.Allow(ctx, "client-ttl")
	if !allowed {
		t.Fatal("expected initial request to be allowed")
	}

	allowed, _ = limiter.Allow(ctx, "client-ttl")
	if allowed {
		t.Fatal("expected second request to be rejected")
	}

	srv.FastForward(61 * time.Second)

	allowed, _ = limiter.Allow(ctx, "client-ttl")
	if !allowed {
		t.Fatal("expected bucket to reset after TTL expiration")
	}
}

func TestRateLimitMiddleware_UsesForwardedClientID(t *testing.T) {
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	limiter := NewRedisRateLimiter(client, 0, 1)
	handler := RateLimit(limiter)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	req1.Header.Set("X-Forwarded-For", "203.0.113.10, 10.0.0.2")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected first forwarded request to pass, got %d", rec1.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("X-Forwarded-For", "203.0.113.10")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("expected second forwarded request to be rate limited, got %d", rec2.Code)
	}
}
