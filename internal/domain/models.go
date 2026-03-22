package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// --- Transaction Status ---

type TransactionStatus string

const (
	TxStatusPending    TransactionStatus = "PENDING"
	TxStatusProcessing TransactionStatus = "PROCESSING"
	TxStatusCompleted  TransactionStatus = "COMPLETED"
	TxStatusFailed     TransactionStatus = "FAILED"
	TxStatusRefunded   TransactionStatus = "REFUNDED"
)

// ValidTransitions defines the state machine for transaction lifecycle.
// Each key maps to the set of states it can transition to.
var ValidTransitions = map[TransactionStatus][]TransactionStatus{
	TxStatusPending:    {TxStatusProcessing},
	TxStatusProcessing: {TxStatusCompleted, TxStatusFailed},
	TxStatusCompleted:  {TxStatusRefunded},
	TxStatusFailed:     {},
	TxStatusRefunded:   {},
}

// CanTransition checks if a transition from one status to another is valid.
func CanTransition(from, to TransactionStatus) bool {
	allowed, exists := ValidTransitions[from]
	if !exists {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

// IsTerminal returns true if the status is a final state.
func (s TransactionStatus) IsTerminal() bool {
	return s == TxStatusFailed || s == TxStatusRefunded
}

// --- Transaction ---

type Transaction struct {
	ID             string            `json:"id"`
	MerchantID     string            `json:"merchant_id"`
	AmountCents    int64             `json:"amount_cents"`
	Currency       string            `json:"currency"`
	Status         TransactionStatus `json:"status"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	Description    string            `json:"description,omitempty"`
	Metadata       json.RawMessage   `json:"metadata,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

// --- Merchant ---

type MerchantStatus string

const (
	MerchantActive   MerchantStatus = "ACTIVE"
	MerchantInactive MerchantStatus = "INACTIVE"
	MerchantSuspend  MerchantStatus = "SUSPENDED"
)

type Merchant struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	APIKey       string         `json:"-"` // never expose in JSON responses
	BalanceCents int64          `json:"balance_cents"`
	Currency     string         `json:"currency"`
	Status       MerchantStatus `json:"status"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

func (m *Merchant) IsActive() bool {
	return m.Status == MerchantActive
}

func formatCents(cents int64) string {
	return fmt.Sprintf("%.2f", float64(cents)/100)
}

// --- Transaction Event (Audit Trail) ---

type TransactionEvent struct {
	ID            int64           `json:"id"`
	TransactionID string          `json:"transaction_id"`
	EventType     string          `json:"event_type"`
	FromStatus    string          `json:"from_status,omitempty"`
	ToStatus      string          `json:"to_status,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// --- Outbox Event (Reliable Event Publishing) ---

type OutboxEvent struct {
	ID        int64           `json:"id"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
	Published bool            `json:"published"`
	CreatedAt time.Time       `json:"created_at"`
}

// --- Pagination ---

type ListParams struct {
	Limit  int
	Offset int
}

func NewListParams(limit, offset int) ListParams {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	return ListParams{Limit: limit, Offset: offset}
}

// --- String representations ---

func (t *Transaction) String() string {
	return fmt.Sprintf("Transaction{id=%s, merchant=%s, amount=%.2f %s, status=%s}",
		t.ID, t.MerchantID, float64(t.AmountCents)/100, t.Currency, t.Status)
}
