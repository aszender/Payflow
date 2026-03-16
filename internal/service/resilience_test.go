package service

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/aszender/payflow/internal/domain"
)

func TestRetry_SucceedsAfterTransientFailures(t *testing.T) {
	attempts := 0

	err := Retry(context.Background(), RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Millisecond,
		Jitter:      0,
		ShouldRetry: isRetryableBankError,
		Sleep: func(context.Context, time.Duration) error {
			return nil
		},
	}, func(context.Context) error {
		attempts++
		if attempts < 3 {
			return domain.ErrBankUnavailable
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected retry to recover, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

func TestRetry_DoesNotRetryPermanentError(t *testing.T) {
	attempts := 0

	err := Retry(context.Background(), RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Millisecond,
		ShouldRetry: isRetryableBankError,
		Sleep: func(context.Context, time.Duration) error {
			return nil
		},
	}, func(context.Context) error {
		attempts++
		return domain.ErrBankRejected
	})
	if !errors.Is(err, domain.ErrBankRejected) {
		t.Fatalf("expected bank rejection, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("expected one attempt, got %d", attempts)
	}
}

func TestRetryDelay_AddsJitter(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	delay := retryDelay(100*time.Millisecond, time.Second, 50*time.Millisecond, 2, random)
	if delay <= 200*time.Millisecond || delay > 250*time.Millisecond {
		t.Fatalf("unexpected jittered delay: %v", delay)
	}
}

func TestCircuitBreaker_OpensAfterFailures(t *testing.T) {
	cb := NewCircuitBreaker(3, time.Second)
	fail := func() error { return errors.New("fail") }

	_ = cb.Execute(fail)
	_ = cb.Execute(fail)
	_ = cb.Execute(fail)

	err := cb.Execute(fail)
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected circuit to be open, got %v", err)
	}
	if cb.State() != CircuitOpen {
		t.Fatalf("expected OPEN, got %s", cb.State())
	}
}

func TestCircuitBreaker_RecoverAfterTimeout(t *testing.T) {
	cb := NewCircuitBreaker(2, 50*time.Millisecond)
	fail := func() error { return errors.New("fail") }
	success := func() error { return nil }

	_ = cb.Execute(fail)
	_ = cb.Execute(fail)

	time.Sleep(75 * time.Millisecond)

	if err := cb.Execute(success); err != nil {
		t.Fatalf("expected half-open success, got %v", err)
	}
	if cb.State() != CircuitClosed {
		t.Fatalf("expected CLOSED, got %s", cb.State())
	}
}
