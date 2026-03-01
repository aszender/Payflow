package domain

import (
	"encoding/json"
	"time"
)

type TransactionStatus struct{
	Pending   string `json:"pending"`
	Completed string `json:"completed"`
	Failed    string `json:"failed"`
}

type Transaction struct {
    ID             string            `json:"id"`
    MerchantID     string            `json:"merchant_id"`
    Amount         float64           `json:"amount"`
    Currency       string            `json:"currency"`
    Status         TransactionStatus `json:"status"`
    IdempotencyKey string            `json:"idempotency_key,omitempty"`
    Description    string            `json:"description,omitempty"`
    Metadata       json.RawMessage   `json:"metadata,omitempty"`
    CreatedAt      time.Time         `json:"created_at"`
    UpdatedAt      time.Time         `json:"updated_at"`
}
