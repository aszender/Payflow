package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/aszender/payflow/internal/domain"
	"github.com/aszender/payflow/internal/repository/mock"
)

func newTestService() *PaymentService {
	return NewPaymentService(PaymentServiceConfig{
		DB:             nil, // mock mode
		Merchants:      mock.NewMerchantRepo(),
		Transactions:   mock.NewTransactionRepo(),
		Events:         mock.NewEventRepo(),
		Outbox:         mock.NewOutboxRepo(),
		Logger:         slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError})),
		BankClient:     &SimulatedBankClient{Latency: 5 * time.Millisecond},
		CircuitBreaker: NewCircuitBreaker(3, 50*time.Millisecond),
		RetryConfig: RetryConfig{
			MaxAttempts: 2,
			BaseDelay:   time.Millisecond,
			MaxDelay:    2 * time.Millisecond,
			Jitter:      0,
		},
		BankTimeout:    5 * time.Second,
		MaxTransaction: 25000,
	})
}

func TestCreateTransaction_Success(t *testing.T) {
	svc := newTestService()
	ctx := context.Background()

	tx, err := svc.CreateTransaction(ctx, CreateTransactionInput{
		MerchantID: "m_001",
		Amount:     150.00,
		Currency:   "CAD",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tx.Status != domain.TxStatusCompleted {
		t.Errorf("expected COMPLETED, got %s", tx.Status)
	}
	if tx.Amount != 150.00 {
		t.Errorf("expected 150.00, got %.2f", tx.Amount)
	}
	if tx.MerchantID != "m_001" {
		t.Errorf("expected m_001, got %s", tx.MerchantID)
	}
	if tx.ID == "" {
		t.Fatal("expected generated transaction ID")
	}
}

func TestCreateTransaction_ValidationErrors(t *testing.T) {
	svc := newTestService()
	ctx := context.Background()

	tests := []struct {
		name    string
		input   CreateTransactionInput
		wantErr error
	}{
		{
			name:    "zero amount",
			input:   CreateTransactionInput{MerchantID: "m_001", Amount: 0, Currency: "CAD"},
			wantErr: domain.ErrInvalidAmount,
		},
		{
			name:    "negative amount",
			input:   CreateTransactionInput{MerchantID: "m_001", Amount: -50, Currency: "CAD"},
			wantErr: domain.ErrInvalidAmount,
		},
		{
			name:    "merchant not found",
			input:   CreateTransactionInput{MerchantID: "m_999", Amount: 100, Currency: "CAD"},
			wantErr: domain.ErrMerchantNotFound,
		},
		{
			name:    "invalid currency",
			input:   CreateTransactionInput{MerchantID: "m_001", Amount: 100, Currency: "EUR"},
			wantErr: domain.ErrInvalidCurrency,
		},
		{
			name:    "exceeds limit",
			input:   CreateTransactionInput{MerchantID: "m_001", Amount: 50000, Currency: "CAD"},
			wantErr: domain.ErrAmountExceedsLimit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.CreateTransaction(ctx, tt.input)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("expected %v, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestCreateTransaction_Idempotency(t *testing.T) {
	svc := newTestService()
	ctx := context.Background()

	input := CreateTransactionInput{
		MerchantID:     "m_001",
		Amount:         100,
		Currency:       "CAD",
		IdempotencyKey: "idem_abc123",
	}

	tx1, err := svc.CreateTransaction(ctx, input)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	tx2, err := svc.CreateTransaction(ctx, input)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if tx1.ID != tx2.ID {
		t.Errorf("idempotency failed: got different IDs %s vs %s", tx1.ID, tx2.ID)
	}
}

func TestRefund_Success(t *testing.T) {
	svc := newTestService()
	ctx := context.Background()

	tx, err := svc.CreateTransaction(ctx, CreateTransactionInput{
		MerchantID: "m_001", Amount: 200, Currency: "CAD",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if tx.Status != domain.TxStatusCompleted {
		t.Fatalf("expected COMPLETED, got %s", tx.Status)
	}

	refunded, err := svc.RefundTransaction(ctx, tx.ID, "customer request")
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if refunded.Status != domain.TxStatusRefunded {
		t.Errorf("expected REFUNDED, got %s", refunded.Status)
	}
}

func TestRefund_CannotRefundPending(t *testing.T) {
	svc := newTestService()
	ctx := context.Background()

	// Directly create a PENDING transaction
	svc.txns.Create(ctx, &domain.Transaction{
		ID: "tx_pending_test", Status: domain.TxStatusPending, MerchantID: "m_001",
	})

	_, err := svc.RefundTransaction(ctx, "tx_pending_test", "test")
	if !errors.Is(err, domain.ErrCannotRefund) {
		t.Errorf("expected ErrCannotRefund, got %v", err)
	}
}

func TestRefund_CannotRefundTwice(t *testing.T) {
	svc := newTestService()
	ctx := context.Background()

	tx, _ := svc.CreateTransaction(ctx, CreateTransactionInput{
		MerchantID: "m_001", Amount: 100, Currency: "CAD",
	})

	_, err := svc.RefundTransaction(ctx, tx.ID, "first refund")
	if err != nil {
		t.Fatalf("first refund: %v", err)
	}

	_, err = svc.RefundTransaction(ctx, tx.ID, "second refund")
	if !errors.Is(err, domain.ErrCannotRefund) {
		t.Errorf("expected ErrCannotRefund for double refund, got %v", err)
	}
}

func TestTransactionHistory(t *testing.T) {
	svc := newTestService()
	ctx := context.Background()

	tx, _ := svc.CreateTransaction(ctx, CreateTransactionInput{
		MerchantID: "m_001", Amount: 100, Currency: "CAD",
	})

	events, err := svc.GetTransactionHistory(ctx, tx.ID)
	if err != nil {
		t.Fatalf("get history: %v", err)
	}

	// Should have: CREATED, STATUS_PROCESSING, STATUS_COMPLETED
	if len(events) < 3 {
		t.Errorf("expected at least 3 events, got %d", len(events))
	}
}

func TestMerchantBalanceUpdated(t *testing.T) {
	svc := newTestService()
	ctx := context.Background()

	// Initial balance is 0
	merchant, _ := svc.GetMerchantBalance(ctx, "m_001")
	if merchant.Balance != 0 {
		t.Fatalf("expected initial balance 0, got %.2f", merchant.Balance)
	}

	// Process a payment
	svc.CreateTransaction(ctx, CreateTransactionInput{
		MerchantID: "m_001", Amount: 250, Currency: "CAD",
	})

	// Balance should increase
	merchant, _ = svc.GetMerchantBalance(ctx, "m_001")
	if merchant.Balance != 250 {
		t.Errorf("expected balance 250, got %.2f", merchant.Balance)
	}

	// Process another
	svc.CreateTransaction(ctx, CreateTransactionInput{
		MerchantID: "m_001", Amount: 100, Currency: "CAD",
	})
	merchant, _ = svc.GetMerchantBalance(ctx, "m_001")
	if merchant.Balance != 350 {
		t.Errorf("expected balance 350, got %.2f", merchant.Balance)
	}
}

// --- State Machine Tests ---

func TestCanTransition(t *testing.T) {
	tests := []struct {
		name string
		from domain.TransactionStatus
		to   domain.TransactionStatus
		want bool
	}{
		{"pending→processing", domain.TxStatusPending, domain.TxStatusProcessing, true},
		{"processing→completed", domain.TxStatusProcessing, domain.TxStatusCompleted, true},
		{"processing→failed", domain.TxStatusProcessing, domain.TxStatusFailed, true},
		{"completed→refunded", domain.TxStatusCompleted, domain.TxStatusRefunded, true},

		{"pending→completed (skip)", domain.TxStatusPending, domain.TxStatusCompleted, false},
		{"failed→completed", domain.TxStatusFailed, domain.TxStatusCompleted, false},
		{"refunded→completed", domain.TxStatusRefunded, domain.TxStatusCompleted, false},
		{"completed→failed", domain.TxStatusCompleted, domain.TxStatusFailed, false},
		{"pending→refunded", domain.TxStatusPending, domain.TxStatusRefunded, false},
		{"failed→refunded", domain.TxStatusFailed, domain.TxStatusRefunded, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := domain.CanTransition(tt.from, tt.to)
			if got != tt.want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}
