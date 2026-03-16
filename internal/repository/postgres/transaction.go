package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/aszender/payflow/internal/domain"
	"github.com/aszender/payflow/internal/repository"
)

type TransactionRepo struct {
	db repository.DBTX
}

func NewTransactionRepo(db repository.DBTX) *TransactionRepo {
	return &TransactionRepo{db: db}
}

func (r *TransactionRepo) WithTx(tx *sql.Tx) repository.TransactionRepository {
	return &TransactionRepo{db: tx}
}

func (r *TransactionRepo) Create(ctx context.Context, t *domain.Transaction) error {
	metadata := t.Metadata
	if len(metadata) == 0 {
		metadata = []byte(`{}`)
	}

	_, err := r.db.ExecContext(ctx,
		`INSERT INTO transactions
			(id, merchant_id, amount, currency, status, idempotency_key, description, metadata, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6,''), NULLIF($7,''), $8, $9, $10)`,
		t.ID, t.MerchantID, t.Amount, t.Currency, t.Status,
		t.IdempotencyKey, t.Description, metadata,
		t.CreatedAt, t.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert transaction: %w", err)
	}
	return nil
}

func (r *TransactionRepo) GetByID(ctx context.Context, id string) (*domain.Transaction, error) {
	t := &domain.Transaction{}
	var idempKey, desc sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT id, merchant_id, amount, currency, status,
				idempotency_key, description, metadata, created_at, updated_at
		 FROM transactions WHERE id = $1`, id,
	).Scan(&t.ID, &t.MerchantID, &t.Amount, &t.Currency, &t.Status,
		&idempKey, &desc, &t.Metadata, &t.CreatedAt, &t.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, domain.ErrTransactionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query transaction %s: %w", id, err)
	}
	if idempKey.Valid {
		t.IdempotencyKey = idempKey.String
	}
	if desc.Valid {
		t.Description = desc.String
	}
	return t, nil
}

func (r *TransactionRepo) GetByIdempotencyKey(ctx context.Context, key string) (*domain.Transaction, error) {
	t := &domain.Transaction{}
	var idempKey, desc sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT id, merchant_id, amount, currency, status,
				idempotency_key, description, metadata, created_at, updated_at
		 FROM transactions WHERE idempotency_key = $1`, key,
	).Scan(&t.ID, &t.MerchantID, &t.Amount, &t.Currency, &t.Status,
		&idempKey, &desc, &t.Metadata, &t.CreatedAt, &t.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, nil // not found is OK — means it's a new request
	}
	if err != nil {
		return nil, fmt.Errorf("query by idempotency key: %w", err)
	}
	if idempKey.Valid {
		t.IdempotencyKey = idempKey.String
	}
	if desc.Valid {
		t.Description = desc.String
	}
	return t, nil
}

func (r *TransactionRepo) UpdateStatus(ctx context.Context, id string, status domain.TransactionStatus) error {
	result, err := r.db.ExecContext(ctx,
		`UPDATE transactions SET status = $1, updated_at = NOW() WHERE id = $2`,
		status, id,
	)
	if err != nil {
		return fmt.Errorf("update transaction status: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return domain.ErrTransactionNotFound
	}
	return nil
}

func (r *TransactionRepo) ListByMerchant(ctx context.Context, merchantID string, params domain.ListParams) ([]*domain.Transaction, int, error) {
	// Get total count
	var total int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM transactions WHERE merchant_id = $1`, merchantID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count transactions: %w", err)
	}

	// Get page
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, merchant_id, amount, currency, status,
				idempotency_key, description, metadata, created_at, updated_at
		 FROM transactions
		 WHERE merchant_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2 OFFSET $3`,
		merchantID, params.Limit, params.Offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("list transactions: %w", err)
	}
	defer rows.Close()

	var transactions []*domain.Transaction
	for rows.Next() {
		t := &domain.Transaction{}
		var idempKey, desc sql.NullString
		if err := rows.Scan(&t.ID, &t.MerchantID, &t.Amount, &t.Currency, &t.Status,
			&idempKey, &desc, &t.Metadata, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan transaction: %w", err)
		}
		if idempKey.Valid {
			t.IdempotencyKey = idempKey.String
		}
		if desc.Valid {
			t.Description = desc.String
		}
		transactions = append(transactions, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate transactions: %w", err)
	}
	return transactions, total, nil
}
