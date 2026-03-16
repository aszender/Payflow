package service

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aszender/payflow/internal/domain"
	//"github.com/aszender/payflow/internal/middleware"
	"github.com/aszender/payflow/internal/repository"
	//"github.com/google/uuid"
)

// PaymentService contains all business logic for processing payments.
// It orchestrates repositories, enforces the state machine, and ensures
// data consistency through database transactions.

type PaymentService struct {
	db             *sql.DB
	merchants      repository.MerchantRepository
	txns           repository.TransactionRepository
	events         repository.EventRepository
	outbox         repository.OutboxRepository
	logger         *slog.Logger
	bank           BankClient
	breaker        *CircuitBreaker
	retry          RetryConfig
	bankTimeout    time.Duration
	maxTransaction float64
}

type PaymentServiceConfig struct {
	DB             *sql.DB
	Merchants      repository.MerchantRepository
	Transactions   repository.TransactionRepository
	Events         repository.EventRepository
	Outbox         repository.OutboxRepository
	Logger         *slog.Logger
	BankClient     BankClient
	CircuitBreaker *CircuitBreaker
	RetryConfig    RetryConfig
	BankTimeout    time.Duration
	MaxTransaction float64
}

func NewPaymentService(cfg PaymentServiceConfig) *PaymentService {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	bankClient := cfg.BankClient
	if bankClient == nil {
		bankClient = &SimulatedBankClient{Latency: 150 * time.Millisecond}
	}

	breaker := cfg.CircuitBreaker
	if breaker == nil {
		breaker = NewCircuitBreaker(3, 5*time.Second)
	}

	retryCfg := cfg.RetryConfig
	if retryCfg.MaxAttempts <= 0 {
		retryCfg.MaxAttempts = 3
	}
	if retryCfg.BaseDelay <= 0 {
		retryCfg.BaseDelay = 100 * time.Millisecond
	}
	if retryCfg.MaxDelay <= 0 {
		retryCfg.MaxDelay = time.Second
	}
	if retryCfg.Jitter <= 0 {
		retryCfg.Jitter = 50 * time.Millisecond
	}
	if retryCfg.ShouldRetry == nil {
		retryCfg.ShouldRetry = isRetryableBankError
	}

	return &PaymentService{
		db:             cfg.DB,
		merchants:      cfg.Merchants,
		txns:           cfg.Transactions,
		events:         cfg.Events,
		outbox:         cfg.Outbox,
		logger:         logger,
		bank:           bankClient,
		breaker:        breaker,
		retry:          retryCfg,
		bankTimeout:    cfg.BankTimeout,
		maxTransaction: cfg.MaxTransaction,
	}
}

// --- Create Transaction ---

type CreateTransactionInput struct {
	MerchantID     string
	Amount         float64
	Currency       string
	IdempotencyKey string
	Description    string
}

func (s *PaymentService) CreateTransaction(ctx context.Context, input CreateTransactionInput) (*domain.Transaction, error) {
	log := s.logger.With(
		//"request_id", middleware.GetRequestID(ctx),
		"merchant_id", input.MerchantID,
		"amount", input.Amount,
	)

	// 1. Idempotency check
	if input.IdempotencyKey != "" {
		existing, err := s.txns.GetByIdempotencyKey(ctx, input.IdempotencyKey)
		if err != nil {
			return nil, fmt.Errorf("idempotency check: %w", err)
		}
		if existing != nil {
			log.Info("idempotent request — returning existing transaction", "tx_id", existing.ID)
			return existing, nil
		}
	}

	// 2. Validate merchant
	merchant, err := s.merchants.GetByID(ctx, input.MerchantID)
	if err != nil {
		return nil, err
	}
	if !merchant.IsActive() {
		return nil, domain.ErrMerchantInactive
	}

	// 3. Validate amount
	if input.Amount <= 0 {
		return nil, domain.ErrInvalidAmount
	}
	if input.Amount > s.maxTransaction {
		return nil, domain.ErrAmountExceedsLimit
	}

	// 4. Validate currency
	if input.Currency != "CAD" && input.Currency != "USD" {
		return nil, domain.ErrInvalidCurrency
	}

	// 5. Create transaction record
	now := time.Now()
	tx := &domain.Transaction{
		ID:             generateTransactionID(),
		MerchantID:     input.MerchantID,
		Amount:         input.Amount,
		Currency:       input.Currency,
		Status:         domain.TxStatusPending,
		IdempotencyKey: input.IdempotencyKey,
		Description:    input.Description,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	// 6. Save within DB transaction (atomic: insert tx + insert event)
	if s.db != nil {
		err = s.createInDBTransaction(ctx, tx)
	} else {
		// Mock mode (testing without real DB)
		err = s.createWithMocks(ctx, tx)
	}
	if err != nil {
		return nil, err
	}

	log.Info("transaction created", "tx_id", tx.ID)

	// 7. Process payment (bank call)
	if err := s.processPayment(ctx, tx, log); err != nil {
		// Return the tx even on failure — client needs the ID to check status
		return tx, nil
	}

	return tx, nil
}

func (s *PaymentService) createInDBTransaction(ctx context.Context, tx *domain.Transaction) error {
	return withTransaction(ctx, s.db, func(dbTx *sql.Tx) error {
		txnRepo := s.txns.WithTx(dbTx)
		eventRepo := s.events.WithTx(dbTx)
		outboxRepo := s.outbox.WithTx(dbTx)

		if err := txnRepo.Create(ctx, tx); err != nil {
			return fmt.Errorf("save transaction: %w", err)
		}

		if err := eventRepo.Create(ctx, &domain.TransactionEvent{
			TransactionID: tx.ID,
			EventType:     "CREATED",
			ToStatus:      string(domain.TxStatusPending),
			CreatedAt:     tx.CreatedAt,
		}); err != nil {
			return fmt.Errorf("save creation event: %w", err)
		}

		// Write to outbox for reliable event publishing
		payload, _ := json.Marshal(map[string]interface{}{
			"transaction_id": tx.ID,
			"merchant_id":    tx.MerchantID,
			"amount":         tx.Amount,
			"currency":       tx.Currency,
			"status":         tx.Status,
		})
		if err := outboxRepo.Create(ctx, &domain.OutboxEvent{
			EventType: "transaction.created",
			Payload:   payload,
			CreatedAt: tx.CreatedAt,
		}); err != nil {
			return fmt.Errorf("save outbox event: %w", err)
		}

		return nil
	})
}

func (s *PaymentService) createWithMocks(ctx context.Context, tx *domain.Transaction) error {
	if err := s.txns.Create(ctx, tx); err != nil {
		return err
	}
	s.events.Create(ctx, &domain.TransactionEvent{
		TransactionID: tx.ID,
		EventType:     "CREATED",
		ToStatus:      string(domain.TxStatusPending),
		CreatedAt:     tx.CreatedAt,
	})
	return nil
}

// --- Process Payment (Bank Interaction) ---

func (s *PaymentService) processPayment(ctx context.Context, tx *domain.Transaction, log *slog.Logger) error {
	// Transition: PENDING → PROCESSING
	if err := s.transition(ctx, tx, domain.TxStatusProcessing); err != nil {
		return err
	}

	// Call bank with timeout
	bankCtx, cancel := context.WithTimeout(ctx, s.bankTimeout)
	defer cancel()

	err := s.callBank(bankCtx, tx)
	if err != nil {
		// Bank failed — mark FAILED
		log.Warn("bank call failed", "tx_id", tx.ID, "error", err)
		s.transition(ctx, tx, domain.TxStatusFailed)
		return err
	}

	// Bank confirmed — mark COMPLETED
	if err := s.transition(ctx, tx, domain.TxStatusCompleted); err != nil {
		return err
	}

	// Credit merchant balance
	if err := s.merchants.UpdateBalance(ctx, tx.MerchantID, tx.Amount); err != nil {
		log.Error("failed to credit merchant", "tx_id", tx.ID, "error", err)
		return err
	}

	log.Info("transaction completed", "tx_id", tx.ID, "amount", tx.Amount)
	return nil
}

func (s *PaymentService) callBank(ctx context.Context, tx *domain.Transaction) error {
	req := BankChargeRequest{
		TransactionID: tx.ID,
		MerchantID:    tx.MerchantID,
		Amount:        tx.Amount,
		Currency:      tx.Currency,
	}

	return s.breaker.Execute(func() error {
		return Retry(ctx, s.retry, func(ctx context.Context) error {
			_, err := s.bank.Charge(ctx, req)
			return err
		})
	})
}

// --- Refund ---

func (s *PaymentService) RefundTransaction(ctx context.Context, txID, reason string) (*domain.Transaction, error) {
	//log := s.logger.With("request_id", middleware.GetRequestID(ctx), "tx_id", txID)

	tx, err := s.txns.GetByID(ctx, txID)
	if err != nil {
		return nil, err
	}

	if !domain.CanTransition(tx.Status, domain.TxStatusRefunded) {
		return nil, fmt.Errorf("%w: current status is %s", domain.ErrCannotRefund, tx.Status)
	}

	if s.db != nil {
		err = s.refundInDBTransaction(ctx, tx, reason)
	} else {
		err = s.refundWithMocks(ctx, tx, reason)
	}
	if err != nil {
		return nil, err
	}

	tx.Status = domain.TxStatusRefunded
	//log.Info("transaction refunded", "amount", tx.Amount)
	return tx, nil
}

func (s *PaymentService) refundInDBTransaction(ctx context.Context, tx *domain.Transaction, reason string) error {
	return withTransaction(ctx, s.db, func(dbTx *sql.Tx) error {
		txnRepo := s.txns.WithTx(dbTx)
		merchantRepo := s.merchants.WithTx(dbTx)
		eventRepo := s.events.WithTx(dbTx)
		outboxRepo := s.outbox.WithTx(dbTx)

		if err := txnRepo.UpdateStatus(ctx, tx.ID, domain.TxStatusRefunded); err != nil {
			return err
		}
		if err := merchantRepo.UpdateBalance(ctx, tx.MerchantID, -tx.Amount); err != nil {
			return err
		}

		payload, _ := json.Marshal(map[string]string{"reason": reason})
		if err := eventRepo.Create(ctx, &domain.TransactionEvent{
			TransactionID: tx.ID,
			EventType:     "REFUNDED",
			FromStatus:    string(domain.TxStatusCompleted),
			ToStatus:      string(domain.TxStatusRefunded),
			Payload:       payload,
			CreatedAt:     time.Now(),
		}); err != nil {
			return err
		}

		outboxPayload, _ := json.Marshal(map[string]interface{}{
			"transaction_id": tx.ID,
			"merchant_id":    tx.MerchantID,
			"amount":         tx.Amount,
			"status":         "REFUNDED",
			"reason":         reason,
		})
		return outboxRepo.Create(ctx, &domain.OutboxEvent{
			EventType: "transaction.refunded",
			Payload:   outboxPayload,
			CreatedAt: time.Now(),
		})
	})
}

func (s *PaymentService) refundWithMocks(ctx context.Context, tx *domain.Transaction, reason string) error {
	if err := s.txns.UpdateStatus(ctx, tx.ID, domain.TxStatusRefunded); err != nil {
		return err
	}
	if err := s.merchants.UpdateBalance(ctx, tx.MerchantID, -tx.Amount); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"reason": reason})
	return s.events.Create(ctx, &domain.TransactionEvent{
		TransactionID: tx.ID,
		EventType:     "REFUNDED",
		FromStatus:    string(domain.TxStatusCompleted),
		ToStatus:      string(domain.TxStatusRefunded),
		Payload:       payload,
		CreatedAt:     time.Now(),
	})
}

// --- Read Operations ---

func (s *PaymentService) GetTransaction(ctx context.Context, id string) (*domain.Transaction, error) {
	return s.txns.GetByID(ctx, id)
}

func (s *PaymentService) GetMerchantBalance(ctx context.Context, merchantID string) (*domain.Merchant, error) {
	return s.merchants.GetByID(ctx, merchantID)
}

func (s *PaymentService) ListTransactions(ctx context.Context, merchantID string, params domain.ListParams) ([]*domain.Transaction, int, error) {
	return s.txns.ListByMerchant(ctx, merchantID, params)
}

func (s *PaymentService) GetTransactionHistory(ctx context.Context, txID string) ([]*domain.TransactionEvent, error) {
	return s.events.ListByTransaction(ctx, txID)
}

// --- State Machine ---

func (s *PaymentService) transition(ctx context.Context, tx *domain.Transaction, to domain.TransactionStatus) error {
	from := tx.Status
	if !domain.CanTransition(from, to) {
		return fmt.Errorf("%w: %s → %s", domain.ErrInvalidTransition, from, to)
	}

	if err := s.txns.UpdateStatus(ctx, tx.ID, to); err != nil {
		return err
	}

	tx.Status = to

	s.events.Create(ctx, &domain.TransactionEvent{
		TransactionID: tx.ID,
		EventType:     fmt.Sprintf("STATUS_%s", to),
		FromStatus:    string(from),
		ToStatus:      string(to),
		CreatedAt:     time.Now(),
	})

	return nil
}

// --- DB Transaction helper (duplicated here to avoid circular import) ---

func withTransaction(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func generateTransactionID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("tx_%d", time.Now().UnixNano())
	}
	return "tx_" + hex.EncodeToString(buf[:])
}

func isRetryableBankError(err error) bool {
	return errors.Is(err, domain.ErrBankTimeout) ||
		errors.Is(err, domain.ErrBankUnavailable) ||
		errors.Is(err, context.DeadlineExceeded)
}
