package service

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aszender/payflow/internal/domain"
	"github.com/aszender/payflow/internal/repository/mock"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
)

type spanCollector struct {
	mu    sync.Mutex
	spans []trace.ReadOnlySpan
}

func (c *spanCollector) ExportSpans(_ context.Context, spans []trace.ReadOnlySpan) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spans = append(c.spans, spans...)
	return nil
}

func (c *spanCollector) Shutdown(context.Context) error { return nil }

func (c *spanCollector) snapshot() []trace.ReadOnlySpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]trace.ReadOnlySpan, len(c.spans))
	copy(out, c.spans)
	return out
}

func withTestTracer(t *testing.T) *spanCollector {
	t.Helper()

	exporter := &spanCollector{}
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)

	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})

	return exporter
}

func spanByName(spans []trace.ReadOnlySpan, name string) trace.ReadOnlySpan {
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	return nil
}

func TestCreateTransaction_EmitsServiceSpans(t *testing.T) {
	exporter := withTestTracer(t)

	svc := NewPaymentService(PaymentServiceConfig{
		DB:             nil,
		Merchants:      mock.NewMerchantRepo(),
		Transactions:   mock.NewTransactionRepo(),
		Events:         mock.NewEventRepo(),
		Outbox:         mock.NewOutboxRepo(),
		Logger:         slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError})),
		BankClient:     &SimulatedBankClient{Latency: 5 * time.Millisecond},
		CircuitBreaker: NewCircuitBreaker(3, 50*time.Millisecond),
		RetryConfig: RetryConfig{
			MaxAttempts: 1,
			BaseDelay:   time.Millisecond,
			MaxDelay:    time.Millisecond,
			Jitter:      0,
		},
		BankTimeout:         time.Second,
		MaxTransactionCents: 2500000,
	})

	tx, err := svc.CreateTransaction(context.Background(), CreateTransactionInput{
		MerchantID:  "m_001",
		AmountCents: 10000,
		Currency:    "CAD",
	})
	if err != nil {
		t.Fatalf("create transaction: %v", err)
	}
	if tx.Status != domain.TxStatusCompleted {
		t.Fatalf("expected completed transaction, got %s", tx.Status)
	}

	spans := exporter.snapshot()
	root := spanByName(spans, "PaymentService.CreateTransaction")
	if root == nil {
		t.Fatalf("expected root span PaymentService.CreateTransaction")
	}
	if root.Parent().IsValid() {
		t.Fatalf("expected root span to have no parent, got %s", root.Parent().SpanID())
	}

	persist := spanByName(spans, "PaymentService.createWithMocks")
	if persist == nil {
		t.Fatalf("expected createWithMocks span")
	}
	persistStep := spanByName(spans, "PaymentService.CreateTransaction.persist")
	if persistStep == nil {
		t.Fatalf("expected CreateTransaction.persist span")
	}
	if persistStep.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Fatalf("expected persist step to be a child of CreateTransaction")
	}
	if persist.Parent().SpanID() != persistStep.SpanContext().SpanID() {
		t.Fatalf("expected createWithMocks to be a child of persist step")
	}

	process := spanByName(spans, "PaymentService.processPayment")
	if process == nil {
		t.Fatalf("expected processPayment span")
	}
	if process.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Fatalf("expected processPayment to be a child of CreateTransaction")
	}

	callBank := spanByName(spans, "PaymentService.callBank")
	if callBank == nil {
		t.Fatalf("expected callBank span")
	}
	if callBank.Parent().SpanID() != process.SpanContext().SpanID() {
		t.Fatalf("expected callBank to be a child of processPayment")
	}
}
