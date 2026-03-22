CREATE TABLE IF NOT EXISTS transactions (
    id TEXT PRIMARY KEY,
    merchant_id TEXT NOT NULL REFERENCES merchants(id),
    amount BIGINT NOT NULL,
    currency TEXT NOT NULL,
    status TEXT NOT NULL,
    idempotency_key TEXT NULL,
    description TEXT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT transactions_amount_check CHECK (amount > 0),
    CONSTRAINT transactions_currency_check CHECK (currency IN ('CAD', 'USD')),
    CONSTRAINT transactions_status_check CHECK (status IN ('PENDING', 'PROCESSING', 'COMPLETED', 'FAILED', 'REFUNDED'))
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_transactions_idempotency_key
    ON transactions (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_transactions_merchant_created_at
    ON transactions (merchant_id, created_at DESC);
