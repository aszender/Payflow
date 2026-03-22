package postgres

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aszender/payflow/internal/domain"
)

func integrationDB(t *testing.T) *DB {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := Connect(ctx, dsn, 10, 5, time.Minute)
	if err != nil {
		t.Fatalf("connect database: %v", err)
	}

	migrationsDir := filepath.Join("..", "..", "..", "migrations")
	if err := RunMigrations(ctx, db.DB, migrationsDir); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	resetDatabase(t, db)
	t.Cleanup(func() {
		_ = db.Close()
	})

	return db
}

func resetDatabase(t *testing.T, db *DB) {
	t.Helper()

	ctx := context.Background()
	_, err := db.ExecContext(ctx, `
		TRUNCATE TABLE transaction_events, transactions, outbox RESTART IDENTITY CASCADE;
		DELETE FROM merchants;
	`)
	if err != nil {
		t.Fatalf("reset database: %v", err)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO merchants (id, name, api_key, balance, currency, status)
		VALUES
			('m_001', 'Maple Sports', 'sk_live_maple_001', 0, 'CAD', 'ACTIVE'),
			('m_002', 'Northern Gaming', 'sk_live_northern_002', 500, 'CAD', 'ACTIVE')
	`)
	if err != nil {
		t.Fatalf("seed merchants: %v", err)
	}
}

func TestMerchantRepo_GetByAPIKey(t *testing.T) {
	db := integrationDB(t)
	repo := NewMerchantRepo(db.DB)

	merchant, err := repo.GetByAPIKey(context.Background(), "sk_live_maple_001")
	if err != nil {
		t.Fatalf("get merchant by api key: %v", err)
	}
	if merchant.ID != "m_001" {
		t.Fatalf("expected m_001, got %s", merchant.ID)
	}
}

func TestTransactionRepo_CreateAndGetByID(t *testing.T) {
	db := integrationDB(t)
	repo := NewTransactionRepo(db.DB)

	tx := &domain.Transaction{
		ID:             "tx_integration_001",
		MerchantID:     "m_001",
		Amount:         125.50,
		Currency:       "CAD",
		Status:         domain.TxStatusPending,
		IdempotencyKey: "integration-order-001",
		Description:    "integration payment",
		Metadata:       json.RawMessage(`{"source":"integration_test"}`),
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}

	if err := repo.Create(context.Background(), tx); err != nil {
		t.Fatalf("create transaction: %v", err)
	}

	got, err := repo.GetByID(context.Background(), tx.ID)
	if err != nil {
		t.Fatalf("get transaction: %v", err)
	}
	if got.ID != tx.ID {
		t.Fatalf("expected %s, got %s", tx.ID, got.ID)
	}

	byKey, err := repo.GetByIdempotencyKey(context.Background(), tx.IdempotencyKey)
	if err != nil {
		t.Fatalf("get by idempotency key: %v", err)
	}
	if byKey == nil || byKey.ID != tx.ID {
		t.Fatalf("expected transaction by idempotency key")
	}
}

func TestEventRepo_CreateAndList(t *testing.T) {
	db := integrationDB(t)
	txRepo := NewTransactionRepo(db.DB)
	eventRepo := NewEventRepo(db.DB)

	tx := &domain.Transaction{
		ID:         "tx_events_001",
		MerchantID: "m_001",
		Amount:     50,
		Currency:   "CAD",
		Status:     domain.TxStatusPending,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}
	if err := txRepo.Create(context.Background(), tx); err != nil {
		t.Fatalf("create transaction: %v", err)
	}

	event := &domain.TransactionEvent{
		TransactionID: tx.ID,
		EventType:     "CREATED",
		ToStatus:      string(domain.TxStatusPending),
		Payload:       json.RawMessage(`{"hello":"world"}`),
		CreatedAt:     time.Now().UTC(),
	}
	if err := eventRepo.Create(context.Background(), event); err != nil {
		t.Fatalf("create event: %v", err)
	}

	events, err := eventRepo.ListByTransaction(context.Background(), tx.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
}

func TestOutboxRepo_CreateFetchAndMarkPublished(t *testing.T) {
	db := integrationDB(t)
	repo := NewOutboxRepo(db.DB)
	otherRepo := NewOutboxRepo(db.DB)

	event := &domain.OutboxEvent{
		EventType: "transaction.created",
		Payload:   json.RawMessage(`{"transaction_id":"tx_001"}`),
		CreatedAt: time.Now().UTC(),
	}
	if err := repo.Create(context.Background(), event); err != nil {
		t.Fatalf("create outbox event: %v", err)
	}

	events, err := repo.FetchUnpublished(context.Background(), 10)
	if err != nil {
		t.Fatalf("fetch unpublished: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 unpublished event, got %d", len(events))
	}

	claimedElsewhere, err := otherRepo.FetchUnpublished(context.Background(), 10)
	if err != nil {
		t.Fatalf("fetch unpublished from second repo: %v", err)
	}
	if len(claimedElsewhere) != 0 {
		t.Fatalf("expected claimed event to be hidden from second repo, got %d rows", len(claimedElsewhere))
	}

	if _, err := db.ExecContext(context.Background(),
		`UPDATE outbox SET published_at = NOW() - INTERVAL '1 second' WHERE id = $1`,
		events[0].ID,
	); err != nil {
		t.Fatalf("expire outbox lease: %v", err)
	}

	reclaimed, err := otherRepo.FetchUnpublished(context.Background(), 10)
	if err != nil {
		t.Fatalf("reclaim unpublished after lease expiry: %v", err)
	}
	if len(reclaimed) != 1 {
		t.Fatalf("expected reclaimed event after lease expiry, got %d", len(reclaimed))
	}
	if reclaimed[0].ID != events[0].ID {
		t.Fatalf("expected reclaimed id %d, got %d", events[0].ID, reclaimed[0].ID)
	}

	if err := repo.MarkPublished(context.Background(), reclaimed[0].ID); err != nil {
		t.Fatalf("mark published: %v", err)
	}

	events, err = repo.FetchUnpublished(context.Background(), 10)
	if err != nil {
		t.Fatalf("fetch unpublished after publish: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected no unpublished events, got %d", len(events))
	}
}
