CREATE TABLE IF NOT EXISTS merchants (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    api_key TEXT NOT NULL UNIQUE,
    balance BIGINT NOT NULL DEFAULT 0,
    currency TEXT NOT NULL DEFAULT 'CAD',
    status TEXT NOT NULL DEFAULT 'ACTIVE',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT merchants_status_check CHECK (status IN ('ACTIVE', 'INACTIVE', 'SUSPENDED')),
    CONSTRAINT merchants_currency_check CHECK (currency IN ('CAD', 'USD'))
);
