package service

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"sync"
	"time"
)

var ErrCircuitOpen = errors.New("circuit breaker is open")

type CircuitState string

const (
	CircuitClosed   CircuitState = "CLOSED"
	CircuitOpen     CircuitState = "OPEN"
	CircuitHalfOpen CircuitState = "HALF_OPEN"
)

type CircuitBreaker struct {
	mu              sync.Mutex
	state           CircuitState
	failures        int
	threshold       int
	resetTimeout    time.Duration
	openedAt        time.Time
	halfOpenRunning bool
}

func NewCircuitBreaker(threshold int, resetTimeout time.Duration) *CircuitBreaker {
	if threshold <= 0 {
		threshold = 3
	}
	if resetTimeout <= 0 {
		resetTimeout = 30 * time.Second
	}

	return &CircuitBreaker{
		state:        CircuitClosed,
		threshold:    threshold,
		resetTimeout: resetTimeout,
	}
}

func (cb *CircuitBreaker) Execute(fn func() error) error {
	if err := cb.beforeRequest(); err != nil {
		return err
	}

	err := fn()
	cb.afterRequest(err)
	return err
}

func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == CircuitOpen && time.Since(cb.openedAt) >= cb.resetTimeout {
		cb.state = CircuitHalfOpen
	}

	return cb.state
}

func (cb *CircuitBreaker) beforeRequest() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitClosed:
		return nil
	case CircuitOpen:
		if time.Since(cb.openedAt) < cb.resetTimeout {
			return ErrCircuitOpen
		}
		cb.state = CircuitHalfOpen
		cb.failures = 0
		cb.halfOpenRunning = true
		return nil
	case CircuitHalfOpen:
		if cb.halfOpenRunning {
			return ErrCircuitOpen
		}
		cb.halfOpenRunning = true
		return nil
	default:
		return nil
	}
}

func (cb *CircuitBreaker) afterRequest(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitClosed:
		if err == nil {
			cb.failures = 0
			return
		}
		cb.failures++
		if cb.failures >= cb.threshold {
			cb.state = CircuitOpen
			cb.openedAt = time.Now()
		}
	case CircuitHalfOpen:
		cb.halfOpenRunning = false
		if err == nil {
			cb.state = CircuitClosed
			cb.failures = 0
			return
		}
		cb.state = CircuitOpen
		cb.openedAt = time.Now()
	}
}

type RetryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      time.Duration
	ShouldRetry func(error) bool
	Sleep       func(context.Context, time.Duration) error
	Rand        *rand.Rand
}

func Retry(ctx context.Context, cfg RetryConfig, fn func(context.Context) error) error {
	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	baseDelay := cfg.BaseDelay
	if baseDelay <= 0 {
		baseDelay = 50 * time.Millisecond
	}

	maxDelay := cfg.MaxDelay
	if maxDelay <= 0 {
		maxDelay = time.Second
	}

	sleep := cfg.Sleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}

	shouldRetry := cfg.ShouldRetry
	if shouldRetry == nil {
		shouldRetry = func(err error) bool { return err != nil }
	}

	random := cfg.Rand
	if random == nil {
		random = rand.New(rand.NewSource(time.Now().UnixNano()))
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := fn(ctx)
		if err == nil {
			return nil
		}
		lastErr = err

		if attempt == maxAttempts || !shouldRetry(err) {
			return err
		}

		delay := retryDelay(baseDelay, maxDelay, cfg.Jitter, attempt, random)
		if err := sleep(ctx, delay); err != nil {
			return err
		}
	}

	return lastErr
}

func retryDelay(baseDelay, maxDelay, jitter time.Duration, attempt int, random *rand.Rand) time.Duration {
	delay := time.Duration(float64(baseDelay) * math.Pow(2, float64(attempt-1)))
	if delay > maxDelay {
		delay = maxDelay
	}

	if jitter <= 0 || random == nil {
		return delay
	}

	return delay + time.Duration(random.Int63n(int64(jitter)))
}
