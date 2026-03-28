package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aszender/payflow/internal/domain"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
)

type traceSpanCollector struct {
	mu    sync.Mutex
	spans []trace.ReadOnlySpan
}

func (c *traceSpanCollector) ExportSpans(_ context.Context, spans []trace.ReadOnlySpan) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spans = append(c.spans, spans...)
	return nil
}

func (c *traceSpanCollector) Shutdown(context.Context) error { return nil }

func (c *traceSpanCollector) snapshot() []trace.ReadOnlySpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]trace.ReadOnlySpan, len(c.spans))
	copy(out, c.spans)
	return out
}

func withRepoTracer(t *testing.T) *traceSpanCollector {
	t.Helper()

	exporter := &traceSpanCollector{}
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)

	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})

	return exporter
}

func spanByName(spans []trace.ReadOnlySpan, name string) trace.ReadOnlySpan {
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	return nil
}

type fakePGDriver struct{}

func (d *fakePGDriver) Open(name string) (driver.Conn, error) {
	return &fakePGConn{}, nil
}

type fakePGConn struct{}

func (c *fakePGConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("prepare not implemented")
}
func (c *fakePGConn) Close() error              { return nil }
func (c *fakePGConn) Begin() (driver.Tx, error) { return nil, fmt.Errorf("begin not implemented") }

func (c *fakePGConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

func (c *fakePGConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	now := time.Date(2025, 3, 1, 10, 30, 0, 0, time.UTC)
	arg0 := func() any {
		if len(args) == 0 {
			return nil
		}
		return args[0].Value
	}()

	switch {
	case strings.Contains(query, "SELECT COUNT(*) FROM transactions"):
		return &fakeRows{columns: []string{"count"}, values: [][]driver.Value{{int64(1)}}}, nil
	case strings.Contains(query, "FROM merchants WHERE api_key = $1"):
		return &fakeRows{
			columns: []string{"id", "name", "api_key", "balance_cents", "currency", "status", "created_at", "updated_at"},
			values: [][]driver.Value{{
				"m_001", "Maple Sports", arg0, int64(25000), "CAD", "ACTIVE", now, now,
			}},
		}, nil
	case strings.Contains(query, "FROM merchants WHERE id = $1"):
		return &fakeRows{
			columns: []string{"id", "name", "api_key", "balance_cents", "currency", "status", "created_at", "updated_at"},
			values: [][]driver.Value{{
				arg0, "Maple Sports", "sk_live_maple_001", int64(25000), "CAD", "ACTIVE", now, now,
			}},
		}, nil
	case strings.Contains(query, "FROM transactions WHERE idempotency_key = $1"):
		return fakeTransactionRows(arg0, now), nil
	case strings.Contains(query, "FROM transactions WHERE id = $1"):
		return fakeTransactionRows(arg0, now), nil
	case strings.Contains(query, "FROM transactions") && strings.Contains(query, "WHERE merchant_id = $1"):
		return &fakeRows{
			columns: []string{"id", "merchant_id", "amount_cents", "currency", "status", "idempotency_key", "description", "metadata", "created_at", "updated_at"},
			values: [][]driver.Value{{
				"tx_list_001", arg0, int64(10000), "CAD", "COMPLETED", nil, "listed", []byte(`{"source":"test"}`), now, now,
			}},
		}, nil
	case strings.Contains(query, "FROM transaction_events"):
		return &fakeRows{
			columns: []string{"id", "transaction_id", "event_type", "from_status", "to_status", "payload", "created_at"},
			values: [][]driver.Value{{
				int64(1), arg0, "CREATED", "", "PENDING", []byte(`{"source":"test"}`), now,
			}},
		}, nil
	case strings.Contains(query, "RETURNING o.id, o.event_type, o.payload, o.published, o.created_at"):
		return &fakeRows{
			columns: []string{"id", "event_type", "payload", "published", "created_at"},
			values: [][]driver.Value{{
				int64(1), "transaction.created", []byte(`{"transaction_id":"tx_001"}`), false, now,
			}},
		}, nil
	default:
		return nil, fmt.Errorf("unexpected query: %s", query)
	}
}

type fakeRows struct {
	columns []string
	values  [][]driver.Value
	idx     int
}

func (r *fakeRows) Columns() []string { return r.columns }
func (r *fakeRows) Close() error      { return nil }

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.idx])
	r.idx++
	return nil
}

func fakeTransactionRows(id any, now time.Time) *fakeRows {
	return &fakeRows{
		columns: []string{"id", "merchant_id", "amount_cents", "currency", "status", "idempotency_key", "description", "metadata", "created_at", "updated_at"},
		values: [][]driver.Value{{
			id, "m_001", int64(10000), "CAD", "PENDING", "idem_001", "test", []byte(`{"source":"test"}`), now, now,
		}},
	}
}

func openTracingDB(t *testing.T) *sql.DB {
	t.Helper()

	driverName := fmt.Sprintf("fake-pg-%d", time.Now().UnixNano())
	sql.Register(driverName, &fakePGDriver{})

	db, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open fake db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	return db
}

func TestPostgresRepos_EmitSpans(t *testing.T) {
	exporter := withRepoTracer(t)
	db := openTracingDB(t)

	merchantRepo := NewMerchantRepo(db)
	txRepo := NewTransactionRepo(db)
	eventRepo := NewEventRepo(db)
	outboxRepo := NewOutboxRepo(db)

	tx := &domain.Transaction{
		ID:             "tx_trace_001",
		MerchantID:     "m_001",
		AmountCents:    10000,
		Currency:       "CAD",
		Status:         domain.TxStatusPending,
		IdempotencyKey: "idem_trace_001",
		Description:    "trace me",
		Metadata:       []byte(`{"source":"test"}`),
		CreatedAt:      time.Date(2025, 3, 1, 10, 30, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2025, 3, 1, 10, 30, 0, 0, time.UTC),
	}

	if err := txRepo.Create(context.Background(), tx); err != nil {
		t.Fatalf("tx create: %v", err)
	}
	if _, err := txRepo.GetByID(context.Background(), tx.ID); err != nil {
		t.Fatalf("tx get by id: %v", err)
	}
	if _, err := txRepo.GetByIdempotencyKey(context.Background(), tx.IdempotencyKey); err != nil {
		t.Fatalf("tx get by idempotency key: %v", err)
	}
	if err := txRepo.UpdateStatus(context.Background(), tx.ID, domain.TxStatusCompleted); err != nil {
		t.Fatalf("tx update status: %v", err)
	}
	if _, _, err := txRepo.ListByMerchant(context.Background(), "m_001", domain.NewListParams(10, 0)); err != nil {
		t.Fatalf("tx list: %v", err)
	}

	if _, err := merchantRepo.GetByAPIKey(context.Background(), "sk_live_maple_001"); err != nil {
		t.Fatalf("merchant get by api key: %v", err)
	}
	if err := merchantRepo.UpdateBalance(context.Background(), "m_001", 5000); err != nil {
		t.Fatalf("merchant update balance: %v", err)
	}

	if err := eventRepo.Create(context.Background(), &domain.TransactionEvent{
		TransactionID: tx.ID,
		EventType:     "CREATED",
		ToStatus:      string(domain.TxStatusPending),
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("event create: %v", err)
	}
	if _, err := eventRepo.ListByTransaction(context.Background(), tx.ID); err != nil {
		t.Fatalf("event list: %v", err)
	}

	if err := outboxRepo.Create(context.Background(), &domain.OutboxEvent{
		EventType: "transaction.created",
		Payload:   []byte(`{"transaction_id":"tx_001"}`),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("outbox create: %v", err)
	}
	if _, err := outboxRepo.FetchUnpublished(context.Background(), 10); err != nil {
		t.Fatalf("outbox fetch: %v", err)
	}
	if err := outboxRepo.MarkPublished(context.Background(), 1); err != nil {
		t.Fatalf("outbox mark published: %v", err)
	}

	spans := exporter.snapshot()
	want := []string{
		"TransactionRepo.Create",
		"TransactionRepo.GetByID",
		"TransactionRepo.GetByIdempotencyKey",
		"TransactionRepo.UpdateStatus",
		"TransactionRepo.ListByMerchant",
		"MerchantRepo.GetByAPIKey",
		"MerchantRepo.UpdateBalance",
		"EventRepo.Create",
		"EventRepo.ListByTransaction",
		"OutboxRepo.Create",
		"OutboxRepo.FetchUnpublished",
		"OutboxRepo.MarkPublished",
	}
	for _, name := range want {
		if spanByName(spans, name) == nil {
			t.Fatalf("expected span %q", name)
		}
	}
}
