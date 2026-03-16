package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/aszender/payflow/internal/domain"
	"github.com/aszender/payflow/internal/repository"
)

type MerchantRepo struct {
	db repository.DBTX
}

func NewMerchantRepo(db repository.DBTX) *MerchantRepo {
	return &MerchantRepo{db: db}
}

func (r *MerchantRepo) WithTx(tx *sql.Tx) repository.MerchantRepository {
	return &MerchantRepo{db: tx}
}

func (r *MerchantRepo) GetByID(ctx context.Context, id string) (*domain.Merchant, error) {
	m := &domain.Merchant{}
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, api_key, balance, currency, status, created_at, updated_at
		 FROM merchants WHERE id = $1`, id,
	).Scan(&m.ID, &m.Name, &m.APIKey, &m.Balance, &m.Currency, &m.Status, &m.CreatedAt, &m.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, domain.ErrMerchantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query merchant %s: %w", id, err)
	}
	return m, nil
}

func (r *MerchantRepo) GetByAPIKey(ctx context.Context, apiKey string) (*domain.Merchant, error) {
	m := &domain.Merchant{}
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, api_key, balance, currency, status, created_at, updated_at
		 FROM merchants WHERE api_key = $1 AND status = 'ACTIVE'`, apiKey,
	).Scan(&m.ID, &m.Name, &m.APIKey, &m.Balance, &m.Currency, &m.Status, &m.CreatedAt, &m.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, domain.ErrMerchantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query merchant by api_key: %w", err)
	}
	return m, nil
}

func (r *MerchantRepo) UpdateBalance(ctx context.Context, id string, delta float64) error {
	result, err := r.db.ExecContext(ctx,
		`UPDATE merchants
		 SET balance = balance + $1, updated_at = NOW()
		 WHERE id = $2 AND (balance + $1) >= 0`, // prevent negative balance
		delta, id,
	)
	if err != nil {
		return fmt.Errorf("update balance for %s: %w", id, err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		// Could be not found OR insufficient funds
		_, err := r.GetByID(ctx, id)
		if err != nil {
			return err // merchant not found
		}
		return domain.ErrInsufficientFunds
	}
	return nil
}
