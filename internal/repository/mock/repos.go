package mock

import (
	"context"
	"database/sql"
	"sync"

	"github.com/aszender/payflow/internal/domain"
	"github.com/aszender/payflow/internal/repository"
)

type MerchantRepo struct {
	mu        sync.RWMutex
	Merchants map[string]*domain.Merchant
}

func NewMerchantRepo() *MerchantRepo {
	return &MerchantRepo{
		Merchants: map[string]*domain.Merchant{
			"m_001": {ID: "m_001", Name: "Maple Sports", APIKey: "sk_live_maple_001", BalanceCents: 0, Currency: "CAD", Status: domain.MerchantActive},
			"m_002": {ID: "m_002", Name: "Northern Gaming", APIKey: "sk_live_northern_002", BalanceCents: 50000, Currency: "CAD", Status: domain.MerchantActive},
		},
	}
}

func (r *MerchantRepo) WithTx(_ *sql.Tx) repository.MerchantRepository { return r }

func (r *MerchantRepo) GetByID(_ context.Context, id string) (*domain.Merchant, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.Merchants[id]
	if !ok {
		return nil, domain.ErrMerchantNotFound
	}
	copy := *m
	return &copy, nil
}

func (r *MerchantRepo) GetByAPIKey(_ context.Context, apiKey string) (*domain.Merchant, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.Merchants {
		if m.APIKey == apiKey {
			copy := *m
			return &copy, nil
		}
	}
	return nil, domain.ErrMerchantNotFound
}

func (r *MerchantRepo) UpdateBalance(_ context.Context, id string, delta int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.Merchants[id]
	if !ok {
		return domain.ErrMerchantNotFound
	}
	if m.BalanceCents+delta < 0 {
		return domain.ErrInsufficientFunds
	}
	m.BalanceCents += delta
	return nil
}

type TransactionRepo struct {
	mu           sync.RWMutex
	Transactions map[string]*domain.Transaction
	order        []string
}

func NewTransactionRepo() *TransactionRepo {
	return &TransactionRepo{
		Transactions: make(map[string]*domain.Transaction),
	}
}

func (r *TransactionRepo) WithTx(_ *sql.Tx) repository.TransactionRepository { return r }

func (r *TransactionRepo) Create(_ context.Context, t *domain.Transaction) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := *t
	r.Transactions[t.ID] = &copy
	r.order = append(r.order, t.ID)
	return nil
}

func (r *TransactionRepo) GetByID(_ context.Context, id string) (*domain.Transaction, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.Transactions[id]
	if !ok {
		return nil, domain.ErrTransactionNotFound
	}
	copy := *t
	return &copy, nil
}

func (r *TransactionRepo) GetByIdempotencyKey(_ context.Context, key string) (*domain.Transaction, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.Transactions {
		if t.IdempotencyKey == key {
			copy := *t
			return &copy, nil
		}
	}
	return nil, nil // not found is OK
}

func (r *TransactionRepo) UpdateStatus(_ context.Context, id string, status domain.TransactionStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.Transactions[id]
	if !ok {
		return domain.ErrTransactionNotFound
	}
	t.Status = status
	return nil
}

func (r *TransactionRepo) ListByMerchant(_ context.Context, merchantID string, params domain.ListParams) ([]*domain.Transaction, int, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var filtered []*domain.Transaction
	for i := len(r.order) - 1; i >= 0; i-- {
		t := r.Transactions[r.order[i]]
		if t.MerchantID == merchantID {
			copy := *t
			filtered = append(filtered, &copy)
		}
	}

	total := len(filtered)
	start := params.Offset
	if start > total {
		start = total
	}
	end := start + params.Limit
	if end > total {
		end = total
	}
	return filtered[start:end], total, nil
}

type EventRepo struct {
	mu     sync.RWMutex
	Events []*domain.TransactionEvent
}

func NewEventRepo() *EventRepo {
	return &EventRepo{}
}

func (r *EventRepo) WithTx(_ *sql.Tx) repository.EventRepository { return r }

func (r *EventRepo) Create(_ context.Context, event *domain.TransactionEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := *event
	r.Events = append(r.Events, &copy)
	return nil
}

func (r *EventRepo) ListByTransaction(_ context.Context, txID string) ([]*domain.TransactionEvent, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*domain.TransactionEvent
	for _, e := range r.Events {
		if e.TransactionID == txID {
			copy := *e
			result = append(result, &copy)
		}
	}
	return result, nil
}

type OutboxRepo struct {
	mu     sync.RWMutex
	Events []*domain.OutboxEvent
	nextID int64
}

func NewOutboxRepo() *OutboxRepo {
	return &OutboxRepo{nextID: 1}
}

func (r *OutboxRepo) WithTx(_ *sql.Tx) repository.OutboxRepository { return r }

func (r *OutboxRepo) Create(_ context.Context, event *domain.OutboxEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	event.ID = r.nextID
	r.nextID++
	copy := *event
	r.Events = append(r.Events, &copy)
	return nil
}

func (r *OutboxRepo) FetchUnpublished(_ context.Context, limit int) ([]*domain.OutboxEvent, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*domain.OutboxEvent
	for _, e := range r.Events {
		if !e.Published && len(result) < limit {
			copy := *e
			result = append(result, &copy)
		}
	}
	return result, nil
}

func (r *OutboxRepo) MarkPublished(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.Events {
		if e.ID == id {
			e.Published = true
			return nil
		}
	}
	return nil
}
