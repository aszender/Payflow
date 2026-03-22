ALTER TABLE merchants ADD COLUMN balance_cents BIGINT NOT NULL DEFAULT 0;
UPDATE merchants SET balance_cents = ROUND(coalesce(balance, 0) * 100);
ALTER TABLE merchants DROP COLUMN IF EXISTS balance;
ALTER TABLE merchants
    ADD CONSTRAINT merchants_balance_cents_check CHECK (balance_cents >= 0);

ALTER TABLE transactions ADD COLUMN amount_cents BIGINT NOT NULL DEFAULT 0;
UPDATE transactions SET amount_cents = ROUND(amount * 100);
ALTER TABLE transactions DROP COLUMN IF EXISTS amount;
ALTER TABLE transactions DROP CONSTRAINT IF EXISTS transactions_amount_check;
ALTER TABLE transactions
    ADD CONSTRAINT transactions_amount_cents_check CHECK (amount_cents > 0);
