package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/aszender/payflow/internal/domain"
	"github.com/aszender/payflow/internal/repository"
)

// --- Event Repository ---

type EventRepo struct {
	db repository.DBTX
}

func NewEventRepo(db repository.DBTX) *EventRepo {
	return &EventRepo{db: db}
}

func (r *EventRepo) WithTx(tx *sql.Tx) repository.EventRepository {
	return &EventRepo{db: tx}
}

func (r *EventRepo) Create(ctx context.Context, event *domain.TransactionEvent) error {
	ctx, span := tracer().Start(ctx, "EventRepo.Create")
	defer span.End()

	_, err := r.db.ExecContext(ctx,
		`INSERT INTO transaction_events
			(transaction_id, event_type, from_status, to_status, payload, created_at)
		 VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''), $5, $6)`,
		event.TransactionID, event.EventType,
		event.FromStatus, event.ToStatus,
		event.Payload, event.CreatedAt,
	)
	if err != nil {
		err := fmt.Errorf("insert event: %w", err)
		recordSpanError(span, err)
		return err
	}
	return nil
}

func (r *EventRepo) ListByTransaction(ctx context.Context, txID string) ([]*domain.TransactionEvent, error) {
	ctx, span := tracer().Start(ctx, "EventRepo.ListByTransaction")
	defer span.End()

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, transaction_id, event_type, 
				COALESCE(from_status,''), COALESCE(to_status,''),
				payload, created_at
		 FROM transaction_events
		 WHERE transaction_id = $1
		 ORDER BY created_at ASC`, txID,
	)
	if err != nil {
		err := fmt.Errorf("list events for tx %s: %w", txID, err)
		recordSpanError(span, err)
		return nil, err
	}
	defer rows.Close()

	var events []*domain.TransactionEvent
	for rows.Next() {
		e := &domain.TransactionEvent{}
		if err := rows.Scan(&e.ID, &e.TransactionID, &e.EventType,
			&e.FromStatus, &e.ToStatus, &e.Payload, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		recordSpanError(span, err)
		return nil, err
	}
	return events, nil
}

// --- Outbox Repository ---

type OutboxRepo struct {
	db repository.DBTX
}

const outboxClaimLease = 30 * time.Second

func NewOutboxRepo(db repository.DBTX) *OutboxRepo {
	return &OutboxRepo{db: db}
}

func (r *OutboxRepo) WithTx(tx *sql.Tx) repository.OutboxRepository {
	return &OutboxRepo{db: tx}
}

func (r *OutboxRepo) Create(ctx context.Context, event *domain.OutboxEvent) error {
	ctx, span := tracer().Start(ctx, "OutboxRepo.Create")
	defer span.End()

	_, err := r.db.ExecContext(ctx,
		`INSERT INTO outbox (event_type, payload, published, created_at)
		 VALUES ($1, $2, FALSE, $3)`,
		event.EventType, event.Payload, event.CreatedAt,
	)
	if err != nil {
		err := fmt.Errorf("insert outbox event: %w", err)
		recordSpanError(span, err)
		return err
	}
	return nil
}

func (r *OutboxRepo) FetchUnpublished(ctx context.Context, limit int) ([]*domain.OutboxEvent, error) {
	ctx, span := tracer().Start(ctx, "OutboxRepo.FetchUnpublished")
	defer span.End()

	rows, err := r.db.QueryContext(ctx,
		`WITH claimable AS (
			SELECT id
			FROM outbox
			WHERE published = FALSE
			  AND (published_at IS NULL OR published_at <= NOW())
			ORDER BY created_at ASC
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox AS o
		SET published_at = NOW() + ($2 * INTERVAL '1 microsecond')
		FROM claimable
		WHERE o.id = claimable.id
		RETURNING o.id, o.event_type, o.payload, o.published, o.created_at`,
		limit, outboxClaimLease.Microseconds(),
	)
	if err != nil {
		err := fmt.Errorf("claim unpublished: %w", err)
		recordSpanError(span, err)
		return nil, err
	}
	defer rows.Close()

	var events []*domain.OutboxEvent
	for rows.Next() {
		e := &domain.OutboxEvent{}
		if err := rows.Scan(&e.ID, &e.EventType, &e.Payload, &e.Published, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan outbox event: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		recordSpanError(span, err)
		return nil, err
	}
	return events, nil
}

func (r *OutboxRepo) MarkPublished(ctx context.Context, id int64) error {
	ctx, span := tracer().Start(ctx, "OutboxRepo.MarkPublished")
	defer span.End()

	_, err := r.db.ExecContext(ctx,
		`UPDATE outbox SET published = TRUE, published_at = NOW() WHERE id = $1`, id,
	)
	if err != nil {
		err := fmt.Errorf("mark published %d: %w", id, err)
		recordSpanError(span, err)
		return err
	}
	return nil
}
