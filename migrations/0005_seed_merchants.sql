INSERT INTO merchants (id, name, api_key, balance, currency, status)
VALUES
    ('m_001', 'Maple Sports', 'sk_live_maple_001', 0, 'CAD', 'ACTIVE'),
    ('m_002', 'Northern Gaming', 'sk_live_northern_002', 50000, 'CAD', 'ACTIVE')
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    api_key = EXCLUDED.api_key,
    balance = EXCLUDED.balance,
    currency = EXCLUDED.currency,
    status = EXCLUDED.status,
    updated_at = NOW();
