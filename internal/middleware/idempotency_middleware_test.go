package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/aszender/payflow/internal/domain"
)

func TestIdempotencyMiddleware_ReturnsCachedResponse(t *testing.T) {
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewIdempotencyStore(client)
	var executed int32

	handler := Idempotency(store)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&executed, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"transaction_id":"tx_123","status":"approved"}`))
	}))

	first := merchantRequest(http.MethodPost, "/api/v1/transactions", "m_001", `{"amount_cents":100}`)
	first.Header.Set("Idempotency-Key", "idem-1")
	firstRec := httptest.NewRecorder()
	handler.ServeHTTP(firstRec, first)

	second := merchantRequest(http.MethodPost, "/api/v1/transactions", "m_001", `{"amount_cents":100}`)
	second.Header.Set("Idempotency-Key", "idem-1")
	secondRec := httptest.NewRecorder()
	handler.ServeHTTP(secondRec, second)

	if got := atomic.LoadInt32(&executed); got != 1 {
		t.Fatalf("expected handler to execute once, got %d", got)
	}
	if secondRec.Code != http.StatusCreated {
		t.Fatalf("expected cached status 201, got %d", secondRec.Code)
	}
	if secondRec.Body.String() != firstRec.Body.String() {
		t.Fatalf("expected identical cached body, got %q vs %q", secondRec.Body.String(), firstRec.Body.String())
	}
}

func TestIdempotencyMiddleware_ConcurrentRequestGetsInProgress(t *testing.T) {
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewIdempotencyStore(client)
	started := make(chan struct{})
	release := make(chan struct{})
	var executed int32

	handler := Idempotency(store)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&executed, 1)
		close(started)
		<-release
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"transaction_id":"tx_456"}`))
	}))

	firstReq := merchantRequest(http.MethodPost, "/api/v1/transactions", "m_001", `{}`)
	firstReq.Header.Set("Idempotency-Key", "idem-2")
	firstRec := httptest.NewRecorder()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		handler.ServeHTTP(firstRec, firstReq)
	}()

	<-started

	secondReq := merchantRequest(http.MethodPost, "/api/v1/transactions", "m_001", `{}`)
	secondReq.Header.Set("Idempotency-Key", "idem-2")
	secondRec := httptest.NewRecorder()
	handler.ServeHTTP(secondRec, secondReq)

	if secondRec.Code != http.StatusConflict {
		t.Fatalf("expected in-progress request to return 409, got %d", secondRec.Code)
	}
	if !strings.Contains(secondRec.Body.String(), "IDEMPOTENCY_IN_PROGRESS") {
		t.Fatalf("expected in-progress error code, got %s", secondRec.Body.String())
	}

	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&executed); got != 1 {
		t.Fatalf("expected one handler execution, got %d", got)
	}
}

func TestIdempotencyMiddleware_ExpiredCompletedEntryAllowsReexecution(t *testing.T) {
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewIdempotencyStore(client)
	var executed int32

	handler := Idempotency(store)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count := atomic.AddInt32(&executed, 1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"count":"` + string(rune('0'+count)) + `"}`))
	}))

	req1 := merchantRequest(http.MethodPost, "/api/v1/transactions", "m_001", `{}`)
	req1.Header.Set("Idempotency-Key", "idem-expire")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	srv.FastForward(idempotencyCompletedTTL + time.Second)

	req2 := merchantRequest(http.MethodPost, "/api/v1/transactions", "m_001", `{}`)
	req2.Header.Set("Idempotency-Key", "idem-expire")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if got := atomic.LoadInt32(&executed); got != 2 {
		t.Fatalf("expected handler to re-execute after TTL expiration, got %d", got)
	}
	if rec2.Code != http.StatusCreated {
		t.Fatalf("expected second request to execute successfully, got %d", rec2.Code)
	}
}

func TestIdempotencyMiddleware_ReleasesKeyAfterFailure(t *testing.T) {
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewIdempotencyStore(client)
	var executed int32

	handler := Idempotency(store)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&executed, 1)
		writeError(w, http.StatusBadRequest, "INVALID", "bad request")
	}))

	for range 2 {
		req := merchantRequest(http.MethodPost, "/api/v1/transactions", "m_001", `{}`)
		req.Header.Set("Idempotency-Key", "idem-fail")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 from handler, got %d", rec.Code)
		}
	}

	if got := atomic.LoadInt32(&executed); got != 2 {
		t.Fatalf("expected failed requests not to be cached, got %d executions", got)
	}
}

func TestIdempotencyMiddleware_FailOpenWhenRedisUnavailable(t *testing.T) {
	client := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  10 * time.Millisecond,
		ReadTimeout:  10 * time.Millisecond,
		WriteTimeout: 10 * time.Millisecond,
	})
	t.Cleanup(func() { _ = client.Close() })

	store := NewIdempotencyStore(client)
	var executed int32
	handler := Idempotency(store)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&executed, 1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	for range 2 {
		req := merchantRequest(http.MethodPost, "/api/v1/transactions", "m_001", `{}`)
		req.Header.Set("Idempotency-Key", "idem-open")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("expected fail-open request to succeed, got %d", rec.Code)
		}
	}

	if got := atomic.LoadInt32(&executed); got != 2 {
		t.Fatalf("expected both requests to execute when Redis is unavailable, got %d", got)
	}
}

func merchantRequest(method, target, merchantID, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	merchant := &domain.Merchant{ID: merchantID, Status: domain.MerchantActive}
	return req.WithContext(context.WithValue(req.Context(), MerchantKey, merchant))
}
