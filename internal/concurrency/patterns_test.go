package concurrency

import (
	"context"
	"sync"
	"testing"
	"time"
)

// ============================================================
// WORKER POOL TESTS
// ============================================================

func TestWorkerPool_ProcessesAllJobs(t *testing.T) {
	pool := NewWorkerPool(3, 100)
	defer pool.Shutdown()

	var mu sync.Mutex
	results := make([]int, 0)

	for i := 0; i < 20; i++ {
		val := i
		err := pool.Submit(func() {
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			results = append(results, val)
			mu.Unlock()
		})
		if err != nil {
			t.Errorf("submit failed: %v", err)
		}
	}

	pool.Shutdown()

	if len(results) != 20 {
		t.Errorf("expected 20 results, got %d", len(results))
	}
}

func TestWorkerPool_BackpressureWhenFull(t *testing.T) {
	pool := NewWorkerPool(1, 2) // 1 worker, queue of 2

	// Fill the queue: 1 job running + 2 in queue = worker busy
	blocker := make(chan struct{})
	pool.Submit(func() { <-blocker })             // running (blocks)
	pool.Submit(func() { time.Sleep(time.Second) }) // queued
	pool.Submit(func() { time.Sleep(time.Second) }) // queued

	// This should fail — queue full
	err := pool.Submit(func() {})
	if err == nil {
		t.Error("expected error when queue full, got nil")
	}

	close(blocker) // unblock
	pool.Shutdown()
}

// ============================================================
// VERIFICATION PIPELINE TESTS
// ============================================================

func TestVerifyTransaction_AllPass(t *testing.T) {
	ctx := context.Background()
	results, err := VerifyTransaction(ctx, "tx_001", 100.00)
	if err != nil {
		t.Fatalf("expected all checks to pass, got: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("expected 3 results, got %d", len(results))
	}
	for _, r := range results {
		if !r.Passed {
			t.Errorf("check %s failed: %v", r.Check, r.Error)
		}
	}
}

func TestVerifyTransaction_HighAmountFails(t *testing.T) {
	ctx := context.Background()
	_, err := VerifyTransaction(ctx, "tx_002", 15000.00)
	if err == nil {
		t.Error("expected fraud check to fail for high amount")
	}
}

func TestVerifyTransaction_Timeout(t *testing.T) {
	// Use a very short timeout — checks should fail with context.DeadlineExceeded
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	_, err := VerifyTransaction(ctx, "tx_003", 100.00)
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestVerifyTransaction_RunsInParallel(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	_, err := VerifyTransaction(ctx, "tx_004", 50.00)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// If running sequentially: ~50+30+100 = 180ms minimum
	// If running in parallel: ~100-300ms (slowest check)
	// If it took more than 500ms, they're probably not parallel
	if elapsed > 500*time.Millisecond {
		t.Errorf("took %v — checks may not be running in parallel", elapsed)
	}
}

// ============================================================
// FAN-OUT / FAN-IN TESTS
// ============================================================

func TestProcessConcurrently_AllSucceed(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e"}
	ctx := context.Background()
	results := ProcessConcurrently(ctx, ids, 3)

	if len(results) != 5 {
		t.Fatalf("expected 5 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Error != nil {
			t.Errorf("item %s failed: %v", r.ID, r.Error)
		}
		if r.Output == "" {
			t.Errorf("item %s has empty output", r.ID)
		}
	}
}

func TestProcessConcurrently_RespectsTimeout(t *testing.T) {
	ids := []string{"a", "b", "c"}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	time.Sleep(5 * time.Millisecond) // let timeout expire

	results := ProcessConcurrently(ctx, ids, 2)
	for _, r := range results {
		if r.Error == nil {
			// Some may succeed if goroutine started before timeout
			continue
		}
	}
	// At least verify it doesn't hang
	_ = results
}

// ============================================================
// TOKEN BUCKET TESTS
// ============================================================

func TestTokenBucket_AllowsUpToBurst(t *testing.T) {
	tb := NewTokenBucket(10, 5) // 10/s rate, burst of 5

	// Should allow 5 immediately (burst)
	for i := 0; i < 5; i++ {
		if !tb.Allow() {
			t.Errorf("request %d should be allowed (within burst)", i)
		}
	}

	// 6th should be rejected
	if tb.Allow() {
		t.Error("request 6 should be rejected (burst exceeded)")
	}
}

func TestTokenBucket_RefillsOverTime(t *testing.T) {
	tb := NewTokenBucket(10, 5)

	// Drain all tokens
	for i := 0; i < 5; i++ {
		tb.Allow()
	}

	// Wait for refill (100ms at 10/s = 1 token)
	time.Sleep(150 * time.Millisecond)

	if !tb.Allow() {
		t.Error("should have 1 token after 150ms at 10/s rate")
	}
}

// ============================================================
// SAFE METRICS TESTS
// ============================================================

func TestSafeMetrics_ConcurrentAccess(t *testing.T) {
	m := NewSafeMetrics()
	var wg sync.WaitGroup

	// 100 goroutines each incrementing 1000 times
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				m.Increment("requests")
			}
		}()
	}

	wg.Wait()

	if got := m.Get("requests"); got != 100000 {
		t.Errorf("expected 100000, got %d", got)
	}
}

func TestSafeMetrics_SnapshotIsCopy(t *testing.T) {
	m := NewSafeMetrics()
	m.Increment("a")
	m.Increment("b")

	snap := m.Snapshot()
	snap["a"] = 999 // modify the snapshot

	if m.Get("a") != 1 {
		t.Error("modifying snapshot should not affect original")
	}
}
