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
	ctx, span := tracer().Start(ctx, "MerchantRepo.GetByID")
	defer span.End()

	m := &domain.Merchant{}
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, api_key, balance_cents, currency, status, created_at, updated_at
		 FROM merchants WHERE id = $1`, id,
	).Scan(&m.ID, &m.Name, &m.APIKey, &m.BalanceCents, &m.Currency, &m.Status, &m.CreatedAt, &m.UpdatedAt)

	if err == sql.ErrNoRows {
		err := domain.ErrMerchantNotFound
		recordSpanError(span, err)
		return nil, err
	}
	if err != nil {
		err := fmt.Errorf("query merchant %s: %w", id, err)
		recordSpanError(span, err)
		return nil, err
	}
	return m, nil
}

func (r *MerchantRepo) GetByAPIKey(ctx context.Context, apiKey string) (*domain.Merchant, error) {
	ctx, span := tracer().Start(ctx, "MerchantRepo.GetByAPIKey")
	defer span.End()

	m := &domain.Merchant{}
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, api_key, balance_cents, currency, status, created_at, updated_at
		 FROM merchants WHERE api_key = $1 AND status = 'ACTIVE'`, apiKey,
	).Scan(&m.ID, &m.Name, &m.APIKey, &m.BalanceCents, &m.Currency, &m.Status, &m.CreatedAt, &m.UpdatedAt)

	if err == sql.ErrNoRows {
		err := domain.ErrMerchantNotFound
		recordSpanError(span, err)
		return nil, err
	}
	if err != nil {
		err := fmt.Errorf("query merchant by api_key: %w", err)
		recordSpanError(span, err)
		return nil, err
	}
	return m, nil
}

func (r *MerchantRepo) UpdateBalance(ctx context.Context, id string, delta int64) error {
	ctx, span := tracer().Start(ctx, "MerchantRepo.UpdateBalance")
	defer span.End()

	result, err := r.db.ExecContext(ctx,
		`UPDATE merchants
		 SET balance_cents = balance_cents + $1, updated_at = NOW()
		 WHERE id = $2 AND (balance_cents + $1) >= 0`, // prevent negative balance
		delta, id,
	)
	if err != nil {
		err := fmt.Errorf("update balance for %s: %w", id, err)
		recordSpanError(span, err)
		return err
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		// Could be not found OR insufficient funds
		_, lookupErr := r.GetByID(ctx, id)
		if lookupErr != nil {
			recordSpanError(span, lookupErr)
			return lookupErr // merchant not found
		}
		insufficientFundsErr := domain.ErrInsufficientFunds
		recordSpanError(span, insufficientFundsErr)
		return insufficientFundsErr
	}
	return nil
}
