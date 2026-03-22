package service

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/aszender/payflow/internal/domain"
	"github.com/aszender/payflow/internal/repository"
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

type countingBankClient struct {
	calls  int
	charge func(context.Context, BankChargeRequest) (*BankChargeResponse, error)
}

func (c *countingBankClient) Charge(ctx context.Context, req BankChargeRequest) (*BankChargeResponse, error) {
	c.calls++
	if c.charge != nil {
		return c.charge(ctx, req)
	}
	return &BankChargeResponse{Approved: true, Code: "APPROVED"}, nil
}

type failingEventRepo struct {
	base            *mock.EventRepo
	failOnEventType string
	err             error
}

func (r *failingEventRepo) WithTx(_ *sql.Tx) repository.EventRepository { return r }

func (r *failingEventRepo) Create(ctx context.Context, event *domain.TransactionEvent) error {
	if event.EventType == r.failOnEventType {
		return r.err
	}
	return r.base.Create(ctx, event)
}

func (r *failingEventRepo) ListByTransaction(ctx context.Context, txID string) ([]*domain.TransactionEvent, error) {
	return r.base.ListByTransaction(ctx, txID)
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

func TestCreateTransaction_ContextCanceled(t *testing.T) {
	svc := newTestService()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tx, err := svc.CreateTransaction(ctx, CreateTransactionInput{
		MerchantID: "m_001",
		Amount:     100,
		Currency:   "CAD",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if tx != nil {
		t.Fatalf("expected no transaction, got %+v", tx)
	}
}

func TestCreateTransaction_IdempotencyConflict(t *testing.T) {
	svc := newTestService()
	ctx := context.Background()

	input := CreateTransactionInput{
		MerchantID:     "m_001",
		Amount:         100,
		Currency:       "CAD",
		IdempotencyKey: "idem_conflict",
		Description:    "first",
	}

	if _, err := svc.CreateTransaction(ctx, input); err != nil {
		t.Fatalf("first call: %v", err)
	}

	_, err := svc.CreateTransaction(ctx, CreateTransactionInput{
		MerchantID:     "m_001",
		Amount:         101,
		Currency:       "CAD",
		IdempotencyKey: "idem_conflict",
		Description:    "first",
	})
	if !errors.Is(err, domain.ErrDuplicateTransaction) {
		t.Fatalf("expected duplicate transaction error, got %v", err)
	}
}

func TestCreateTransaction_BankTimeoutPropagatesAndMarksFailed(t *testing.T) {
	merchantRepo := mock.NewMerchantRepo()
	txRepo := mock.NewTransactionRepo()
	eventRepo := mock.NewEventRepo()
	outboxRepo := mock.NewOutboxRepo()

	svc := NewPaymentService(PaymentServiceConfig{
		Merchants:      merchantRepo,
		Transactions:   txRepo,
		Events:         eventRepo,
		Outbox:         outboxRepo,
		Logger:         slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError})),
		BankClient:     &SimulatedBankClient{Latency: 50 * time.Millisecond},
		CircuitBreaker: NewCircuitBreaker(3, 50*time.Millisecond),
		RetryConfig: RetryConfig{
			MaxAttempts: 2,
			BaseDelay:   time.Millisecond,
			MaxDelay:    time.Millisecond,
			Jitter:      0,
		},
		BankTimeout:    time.Millisecond,
		MaxTransaction: 25000,
	})

	tx, err := svc.CreateTransaction(context.Background(), CreateTransactionInput{
		MerchantID: "m_001",
		Amount:     100,
		Currency:   "CAD",
	})
	if !errors.Is(err, domain.ErrBankTimeout) {
		t.Fatalf("expected bank timeout, got %v", err)
	}
	if tx == nil {
		t.Fatal("expected transaction to be returned on processing failure")
	}
	if tx.Status != domain.TxStatusFailed {
		t.Fatalf("expected FAILED status, got %s", tx.Status)
	}

	merchant, err := merchantRepo.GetByID(context.Background(), "m_001")
	if err != nil {
		t.Fatalf("merchant lookup: %v", err)
	}
	if merchant.Balance != 0 {
		t.Fatalf("expected unchanged balance, got %.2f", merchant.Balance)
	}

	events, err := eventRepo.ListByTransaction(context.Background(), tx.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 lifecycle events, got %d", len(events))
	}

	if len(outboxRepo.Events) != 2 {
		t.Fatalf("expected 2 outbox events, got %d", len(outboxRepo.Events))
	}
	if outboxRepo.Events[0].EventType != "transaction.created" {
		t.Fatalf("expected created outbox event, got %s", outboxRepo.Events[0].EventType)
	}
	if outboxRepo.Events[1].EventType != "transaction.failed" {
		t.Fatalf("expected failed outbox event, got %s", outboxRepo.Events[1].EventType)
	}
}

func TestCreateTransaction_TransitionEventFailurePropagates(t *testing.T) {
	merchantRepo := mock.NewMerchantRepo()
	txRepo := mock.NewTransactionRepo()
	eventErr := errors.New("event store unavailable")
	eventRepo := &failingEventRepo{
		base:            mock.NewEventRepo(),
		failOnEventType: "STATUS_PROCESSING",
		err:             eventErr,
	}
	outboxRepo := mock.NewOutboxRepo()
	bankClient := &countingBankClient{}

	svc := NewPaymentService(PaymentServiceConfig{
		Merchants:      merchantRepo,
		Transactions:   txRepo,
		Events:         eventRepo,
		Outbox:         outboxRepo,
		Logger:         slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError})),
		BankClient:     bankClient,
		CircuitBreaker: NewCircuitBreaker(3, 50*time.Millisecond),
		RetryConfig: RetryConfig{
			MaxAttempts: 1,
			BaseDelay:   time.Millisecond,
			MaxDelay:    time.Millisecond,
			Jitter:      0,
		},
		BankTimeout:    time.Second,
		MaxTransaction: 25000,
	})

	tx, err := svc.CreateTransaction(context.Background(), CreateTransactionInput{
		MerchantID: "m_001",
		Amount:     100,
		Currency:   "CAD",
	})
	if !errors.Is(err, eventErr) {
		t.Fatalf("expected event failure, got %v", err)
	}
	if tx == nil {
		t.Fatal("expected transaction to be returned on processing failure")
	}
	if tx.Status != domain.TxStatusProcessing {
		t.Fatalf("expected PROCESSING status, got %s", tx.Status)
	}
	if bankClient.calls != 0 {
		t.Fatalf("expected bank client not to be called, got %d calls", bankClient.calls)
	}
	if len(outboxRepo.Events) != 1 {
		t.Fatalf("expected only created outbox event, got %d", len(outboxRepo.Events))
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

func TestRefund_EmitsOutboxEvent(t *testing.T) {
	merchantRepo := mock.NewMerchantRepo()
	txRepo := mock.NewTransactionRepo()
	eventRepo := mock.NewEventRepo()
	outboxRepo := mock.NewOutboxRepo()

	svc := NewPaymentService(PaymentServiceConfig{
		Merchants:      merchantRepo,
		Transactions:   txRepo,
		Events:         eventRepo,
		Outbox:         outboxRepo,
		Logger:         slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError})),
		BankClient:     &SimulatedBankClient{Latency: time.Millisecond},
		CircuitBreaker: NewCircuitBreaker(3, 50*time.Millisecond),
		RetryConfig: RetryConfig{
			MaxAttempts: 1,
			BaseDelay:   time.Millisecond,
			MaxDelay:    time.Millisecond,
			Jitter:      0,
		},
		BankTimeout:    time.Second,
		MaxTransaction: 25000,
	})

	tx, err := svc.CreateTransaction(context.Background(), CreateTransactionInput{
		MerchantID: "m_001",
		Amount:     200,
		Currency:   "CAD",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.RefundTransaction(context.Background(), tx.ID, "customer request"); err != nil {
		t.Fatalf("refund: %v", err)
	}

	last := outboxRepo.Events[len(outboxRepo.Events)-1]
	if last.EventType != "transaction.refunded" {
		t.Fatalf("expected refunded outbox event, got %s", last.EventType)
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
