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
	"github.com/aszender/payflow/internal/repository"
)

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

type CreateTransactionInput struct {
	MerchantID     string
	Amount         float64
	Currency       string
	IdempotencyKey string
	Description    string
}

func (s *PaymentService) CreateTransaction(ctx context.Context, input CreateTransactionInput) (*domain.Transaction, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}

	log := s.logger.With(
		"merchant_id", input.MerchantID,
		"amount", input.Amount,
	)

	if input.IdempotencyKey != "" {
		existing, err := s.txns.GetByIdempotencyKey(ctx, input.IdempotencyKey)
		if err != nil {
			return nil, fmt.Errorf("idempotency check: %w", err)
		}
		if existing != nil {
			if !matchesIdempotentRequest(existing, input) {
				return nil, fmt.Errorf("%w: idempotency key reused with different request", domain.ErrDuplicateTransaction)
			}
			log.Info("idempotent request — returning existing transaction", "tx_id", existing.ID)
			return existing, nil
		}
	}

	merchant, err := s.merchants.GetByID(ctx, input.MerchantID)
	if err != nil {
		return nil, err
	}
	if !merchant.IsActive() {
		return nil, domain.ErrMerchantInactive
	}

	if input.Amount <= 0 {
		return nil, domain.ErrInvalidAmount
	}
	if input.Amount > s.maxTransaction {
		return nil, domain.ErrAmountExceedsLimit
	}

	if input.Currency != "CAD" && input.Currency != "USD" {
		return nil, domain.ErrInvalidCurrency
	}

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

	if s.db != nil {
		err = s.createInDBTransaction(ctx, tx)
	} else {
		err = s.createWithMocks(ctx, tx)
	}
	if err != nil {
		return nil, err
	}

	log.Info("transaction created", "tx_id", tx.ID)

	if err := s.processPayment(ctx, tx, log); err != nil {
		return tx, err
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

		payload, err := marshalOutboxPayload(map[string]interface{}{
			"transaction_id": tx.ID,
			"merchant_id":    tx.MerchantID,
			"amount":         tx.Amount,
			"currency":       tx.Currency,
			"status":         tx.Status,
		})
		if err != nil {
			return fmt.Errorf("marshal created outbox payload: %w", err)
		}
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
	if err := s.events.Create(ctx, &domain.TransactionEvent{
		TransactionID: tx.ID,
		EventType:     "CREATED",
		ToStatus:      string(domain.TxStatusPending),
		CreatedAt:     tx.CreatedAt,
	}); err != nil {
		return fmt.Errorf("save creation event: %w", err)
	}

	return s.emitOutboxEvent(ctx, "transaction.created", tx, tx.CreatedAt, nil)
}

func (s *PaymentService) processPayment(ctx context.Context, tx *domain.Transaction, log *slog.Logger) error {
	if err := contextError(ctx); err != nil {
		return err
	}

	if err := s.transition(ctx, tx, domain.TxStatusProcessing); err != nil {
		return fmt.Errorf("set processing status: %w", err)
	}

	bankCtx, cancel := bankCallContext(ctx, s.bankTimeout)
	defer cancel()

	err := s.callBank(ctx, bankCtx, tx)
	if err != nil {
		log.Warn("bank call failed", "tx_id", tx.ID, "error", err)
		if transitionErr := s.transition(postBankContext(ctx), tx, domain.TxStatusFailed); transitionErr != nil {
			return errors.Join(err, fmt.Errorf("set failed status: %w", transitionErr))
		}
		return err
	}

	stateCtx := postBankContext(ctx)

	if err := s.completePayment(stateCtx, tx); err != nil {
		log.Error("failed to finalize completed payment", "tx_id", tx.ID, "error", err)
		return err
	}

	log.Info("transaction completed", "tx_id", tx.ID, "amount", tx.Amount)
	return nil
}

func (s *PaymentService) completePayment(ctx context.Context, tx *domain.Transaction) error {
	if s.db != nil {
		return s.completeInDBTransaction(ctx, tx)
	}
	return s.completeWithMocks(ctx, tx)
}

func (s *PaymentService) completeInDBTransaction(ctx context.Context, tx *domain.Transaction) error {
	if err := contextError(ctx); err != nil {
		return err
	}

	from := tx.Status
	to := domain.TxStatusCompleted
	if !domain.CanTransition(from, to) {
		return fmt.Errorf("%w: %s → %s", domain.ErrInvalidTransition, from, to)
	}

	completedAt := time.Now()
	completedTx := *tx
	completedTx.Status = to

	err := withTransaction(ctx, s.db, func(dbTx *sql.Tx) error {
		txnRepo := s.txns.WithTx(dbTx)
		merchantRepo := s.merchants.WithTx(dbTx)
		eventRepo := s.events.WithTx(dbTx)
		outboxRepo := s.outbox.WithTx(dbTx)

		if err := txnRepo.UpdateStatus(ctx, tx.ID, to); err != nil {
			return fmt.Errorf("set completed status: %w", err)
		}
		if err := merchantRepo.UpdateBalance(ctx, tx.MerchantID, tx.Amount); err != nil {
			return fmt.Errorf("credit merchant balance: %w", err)
		}
		if err := eventRepo.Create(ctx, &domain.TransactionEvent{
			TransactionID: tx.ID,
			EventType:     fmt.Sprintf("STATUS_%s", to),
			FromStatus:    string(from),
			ToStatus:      string(to),
			CreatedAt:     completedAt,
		}); err != nil {
			return fmt.Errorf("record transition event: %w", err)
		}

		payload, err := transactionOutboxPayload(&completedTx, nil)
		if err != nil {
			return fmt.Errorf("marshal completed outbox payload: %w", err)
		}
		if err := outboxRepo.Create(ctx, &domain.OutboxEvent{
			EventType: "transaction.completed",
			Payload:   payload,
			CreatedAt: completedAt,
		}); err != nil {
			return fmt.Errorf("record transition outbox event: %w", err)
		}

		return nil
	})
	if err != nil {
		return err
	}

	tx.Status = to
	return nil
}

func (s *PaymentService) completeWithMocks(ctx context.Context, tx *domain.Transaction) error {
	if err := contextError(ctx); err != nil {
		return err
	}

	if err := s.merchants.UpdateBalance(ctx, tx.MerchantID, tx.Amount); err != nil {
		return fmt.Errorf("credit merchant balance: %w", err)
	}

	if err := s.transition(ctx, tx, domain.TxStatusCompleted); err != nil {
		rollbackErr := s.merchants.UpdateBalance(ctx, tx.MerchantID, -tx.Amount)
		if rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("rollback credited balance: %w", rollbackErr))
		}
		return err
	}

	return nil
}

func (s *PaymentService) callBank(parentCtx, bankCtx context.Context, tx *domain.Transaction) error {
	req := BankChargeRequest{
		TransactionID: tx.ID,
		MerchantID:    tx.MerchantID,
		Amount:        tx.Amount,
		Currency:      tx.Currency,
	}

	err := s.breaker.Execute(func() error {
		return Retry(bankCtx, s.retry, func(attemptCtx context.Context) error {
			_, err := s.bank.Charge(attemptCtx, req)
			return err
		})
	})

	return normalizeBankError(parentCtx, bankCtx, err)
}

func (s *PaymentService) RefundTransaction(ctx context.Context, txID, reason string) (*domain.Transaction, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}

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

		payload, err := marshalOutboxPayload(map[string]string{"reason": reason})
		if err != nil {
			return fmt.Errorf("marshal refund payload: %w", err)
		}
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

		outboxPayload, err := marshalOutboxPayload(map[string]interface{}{
			"transaction_id": tx.ID,
			"merchant_id":    tx.MerchantID,
			"amount":         tx.Amount,
			"status":         "REFUNDED",
			"reason":         reason,
		})
		if err != nil {
			return fmt.Errorf("marshal refunded outbox payload: %w", err)
		}
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
	tx.Status = domain.TxStatusRefunded
	if err := s.merchants.UpdateBalance(ctx, tx.MerchantID, -tx.Amount); err != nil {
		return err
	}
	payload, err := marshalOutboxPayload(map[string]string{"reason": reason})
	if err != nil {
		return fmt.Errorf("marshal refund payload: %w", err)
	}
	if err := s.events.Create(ctx, &domain.TransactionEvent{
		TransactionID: tx.ID,
		EventType:     "REFUNDED",
		FromStatus:    string(domain.TxStatusCompleted),
		ToStatus:      string(domain.TxStatusRefunded),
		Payload:       payload,
		CreatedAt:     time.Now(),
	}); err != nil {
		return err
	}

	return s.emitOutboxEvent(ctx, "transaction.refunded", tx, time.Now(), map[string]interface{}{
		"reason": reason,
	})
}

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

func (s *PaymentService) transition(ctx context.Context, tx *domain.Transaction, to domain.TransactionStatus) error {
	if err := contextError(ctx); err != nil {
		return err
	}

	from := tx.Status
	if !domain.CanTransition(from, to) {
		return fmt.Errorf("%w: %s → %s", domain.ErrInvalidTransition, from, to)
	}

	if err := s.txns.UpdateStatus(ctx, tx.ID, to); err != nil {
		return err
	}

	transitionedTx := *tx
	transitionedTx.Status = to

	if err := s.events.Create(ctx, &domain.TransactionEvent{
		TransactionID: tx.ID,
		EventType:     fmt.Sprintf("STATUS_%s", to),
		FromStatus:    string(from),
		ToStatus:      string(to),
		CreatedAt:     time.Now(),
	}); err != nil {
		return s.rollbackTransition(ctx, tx, from, fmt.Errorf("record transition event: %w", err))
	}

	if err := s.emitTransitionOutboxEvent(ctx, &transitionedTx, to); err != nil {
		return s.rollbackTransition(ctx, tx, from, fmt.Errorf("record transition outbox event: %w", err))
	}

	tx.Status = to
	return nil
}

func (s *PaymentService) rollbackTransition(ctx context.Context, tx *domain.Transaction, from domain.TransactionStatus, cause error) error {
	if rollbackErr := s.txns.UpdateStatus(ctx, tx.ID, from); rollbackErr != nil {
		return errors.Join(cause, fmt.Errorf("rollback status to %s: %w", from, rollbackErr))
	}
	tx.Status = from
	return cause
}

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

func matchesIdempotentRequest(existing *domain.Transaction, input CreateTransactionInput) bool {
	if existing == nil {
		return false
	}

	return existing.MerchantID == input.MerchantID &&
		existing.Amount == input.Amount &&
		existing.Currency == input.Currency &&
		existing.Description == input.Description
}

func bankCallContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func postBankContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

func normalizeBankError(parentCtx, bankCtx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) && parentCtx != nil && errors.Is(parentCtx.Err(), context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if parentCtx != nil && errors.Is(parentCtx.Err(), context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		if bankCtx != nil && errors.Is(bankCtx.Err(), context.DeadlineExceeded) {
			return domain.ErrBankTimeout
		}
	}
	return err
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func (s *PaymentService) emitTransitionOutboxEvent(ctx context.Context, tx *domain.Transaction, to domain.TransactionStatus) error {
	switch to {
	case domain.TxStatusCompleted:
		return s.emitOutboxEvent(ctx, "transaction.completed", tx, time.Now(), nil)
	case domain.TxStatusFailed:
		return s.emitOutboxEvent(ctx, "transaction.failed", tx, time.Now(), nil)
	default:
		return nil
	}
}

func (s *PaymentService) emitOutboxEvent(ctx context.Context, eventType string, tx *domain.Transaction, createdAt time.Time, extra map[string]interface{}) error {
	if s.outbox == nil || tx == nil {
		return nil
	}

	payload, err := transactionOutboxPayload(tx, extra)
	if err != nil {
		return err
	}

	return s.outbox.Create(ctx, &domain.OutboxEvent{
		EventType: eventType,
		Payload:   payload,
		CreatedAt: createdAt,
	})
}

func transactionOutboxPayload(tx *domain.Transaction, extra map[string]interface{}) (json.RawMessage, error) {
	payload := map[string]interface{}{
		"transaction_id": tx.ID,
		"merchant_id":    tx.MerchantID,
		"amount":         tx.Amount,
		"currency":       tx.Currency,
		"status":         tx.Status,
	}
	for key, value := range extra {
		payload[key] = value
	}

	return marshalOutboxPayload(payload)
}

func marshalOutboxPayload(payload interface{}) (json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return raw, nil
}
