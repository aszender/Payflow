-- initial schema
CREATE TABLE IF NOT EXISTS payments (
    id TEXT PRIMARY KEY,
    amount BIGINT NOT NULL,
    status TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now()
);
