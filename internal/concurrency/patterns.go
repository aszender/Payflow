package concurrency

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

type WorkerPool struct {
	jobs    chan func()
	slots   chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	workers int
	closed  bool
}

func NewWorkerPool(workers, queueSize int) *WorkerPool {
	wp := &WorkerPool{
		jobs:    make(chan func(), queueSize),
		slots:   make(chan struct{}, workers+queueSize),
		workers: workers,
	}
	for i := 0; i < workers; i++ {
		wp.wg.Add(1)
		go wp.worker(i)
	}
	return wp
}

func (wp *WorkerPool) worker(id int) {
	defer wp.wg.Done()
	for job := range wp.jobs {
		job()
	}
}

func (wp *WorkerPool) Submit(job func()) error {
	wp.mu.Lock()
	defer wp.mu.Unlock()

	if wp.closed {
		return fmt.Errorf("worker pool is shut down")
	}

	select {
	case wp.slots <- struct{}{}:
		wp.jobs <- func() {
			defer func() { <-wp.slots }()
			job()
		}
		return nil
	default:
		return fmt.Errorf("worker pool queue full")
	}
}

func (wp *WorkerPool) Shutdown() {
	wp.mu.Lock()
	if !wp.closed {
		wp.closed = true
		close(wp.jobs)
	}
	wp.mu.Unlock()

	wp.wg.Wait()
}

type VerificationResult struct {
	Check   string
	Passed  bool
	Latency time.Duration
	Error   error
}

func VerifyTransaction(ctx context.Context, txID string, amount float64) ([]VerificationResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	checks := []struct {
		name string
		fn   func(context.Context, string, float64) error
	}{
		{"fraud", checkFraud},
		{"sanctions", checkSanctions},
		{"bank", checkBank},
	}

	resultCh := make(chan VerificationResult, len(checks))
	var wg sync.WaitGroup

	for _, check := range checks {
		wg.Add(1)
		go func(name string, fn func(context.Context, string, float64) error) {
			defer wg.Done()
			start := time.Now()
			err := fn(ctx, txID, amount)
			resultCh <- VerificationResult{
				Check:   name,
				Passed:  err == nil,
				Latency: time.Since(start),
				Error:   err,
			}
		}(check.name, check.fn)
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	var results []VerificationResult
	for r := range resultCh {
		results = append(results, r)
		if !r.Passed {
			cancel() // cancel remaining checks if one fails
		}
	}

	for _, r := range results {
		if !r.Passed {
			return results, fmt.Errorf("verification failed: %s: %v", r.Check, r.Error)
		}
	}
	return results, nil
}

func checkFraud(ctx context.Context, txID string, amount float64) error {
	select {
	case <-time.After(time.Duration(50+rand.Intn(100)) * time.Millisecond):
		if amount > 10000 {
			return fmt.Errorf("high-risk amount: %.2f", amount)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func checkSanctions(ctx context.Context, txID string, amount float64) error {
	select {
	case <-time.After(time.Duration(30+rand.Intn(80)) * time.Millisecond):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func checkBank(ctx context.Context, txID string, amount float64) error {
	select {
	case <-time.After(time.Duration(100+rand.Intn(200)) * time.Millisecond):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type ProcessResult struct {
	ID     string
	Output string
	Error  error
}

func ProcessConcurrently(ctx context.Context, ids []string, concurrency int) []ProcessResult {
	results := make([]ProcessResult, len(ids))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, itemID string) {
			defer wg.Done()
			defer func() { <-sem }()

			select {
			case <-ctx.Done():
				results[idx] = ProcessResult{ID: itemID, Error: ctx.Err()}
				return
			default:
			}

			time.Sleep(time.Duration(50+rand.Intn(100)) * time.Millisecond)
			results[idx] = ProcessResult{
				ID:     itemID,
				Output: fmt.Sprintf("processed_%s", itemID),
			}
		}(i, id)
	}

	wg.Wait()
	return results
}

type TokenBucket struct {
	mu        sync.Mutex
	tokens    float64
	maxTokens float64
	rate      float64
	lastCheck time.Time
}

func NewTokenBucket(rate, burst float64) *TokenBucket {
	return &TokenBucket{
		tokens:    burst,
		maxTokens: burst,
		rate:      rate,
		lastCheck: time.Now(),
	}
}

func (tb *TokenBucket) Allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.lastCheck).Seconds()
	tb.tokens += elapsed * tb.rate
	if tb.tokens > tb.maxTokens {
		tb.tokens = tb.maxTokens
	}
	tb.lastCheck = now

	if tb.tokens < 1 {
		return false
	}
	tb.tokens--
	return true
}

type SafeMetrics struct {
	mu     sync.RWMutex
	counts map[string]int64
}

func NewSafeMetrics() *SafeMetrics {
	return &SafeMetrics{counts: make(map[string]int64)}
}

func (m *SafeMetrics) Increment(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[key]++
}

func (m *SafeMetrics) Get(key string) int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.counts[key]
}

func (m *SafeMetrics) Snapshot() map[string]int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	snap := make(map[string]int64, len(m.counts))
	for k, v := range m.counts {
		snap[k] = v
	}
	return snap
}
