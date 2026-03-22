package repository

import (
	"context"
	"database/sql"

	"github.com/aszender/payflow/internal/domain"
)

type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

type MerchantRepository interface {
	GetByID(ctx context.Context, id string) (*domain.Merchant, error)
	GetByAPIKey(ctx context.Context, apiKey string) (*domain.Merchant, error)
	UpdateBalance(ctx context.Context, id string, delta float64) error

	WithTx(tx *sql.Tx) MerchantRepository
}

type TransactionRepository interface {
	Create(ctx context.Context, tx *domain.Transaction) error
	GetByID(ctx context.Context, id string) (*domain.Transaction, error)
	GetByIdempotencyKey(ctx context.Context, key string) (*domain.Transaction, error)
	UpdateStatus(ctx context.Context, id string, status domain.TransactionStatus) error
	ListByMerchant(ctx context.Context, merchantID string, params domain.ListParams) ([]*domain.Transaction, int, error)

	WithTx(tx *sql.Tx) TransactionRepository
}

type EventRepository interface {
	Create(ctx context.Context, event *domain.TransactionEvent) error
	ListByTransaction(ctx context.Context, txID string) ([]*domain.TransactionEvent, error)

	WithTx(tx *sql.Tx) EventRepository
}

type OutboxRepository interface {
	Create(ctx context.Context, event *domain.OutboxEvent) error
	// FetchUnpublished atomically claims a batch of unpublished events for a short lease.
	// Claimed rows should not be returned again until the lease expires or they are marked published.
	FetchUnpublished(ctx context.Context, limit int) ([]*domain.OutboxEvent, error)
	MarkPublished(ctx context.Context, id int64) error

	WithTx(tx *sql.Tx) OutboxRepository
}
