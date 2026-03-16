package service

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/aszender/payflow/internal/domain"
	"github.com/aszender/payflow/internal/repository"
)

type OutboxWorkerConfig struct {
	Outbox    repository.OutboxRepository
	Brokers   []string
	Topic     string
	Logger    *slog.Logger
	Interval  time.Duration
	BatchSize int
}

type outboxPublisher interface {
	Publish(context.Context, *domain.OutboxEvent) error
	Close() error
}

type OutboxWorker struct {
	outbox    repository.OutboxRepository
	publisher outboxPublisher
	logger    *slog.Logger
	interval  time.Duration
	batchSize int
}

func NewOutboxWorker(cfg OutboxWorkerConfig) *OutboxWorker {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	interval := cfg.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}

	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 100
	}

	return &OutboxWorker{
		outbox:    cfg.Outbox,
		publisher: newOutboxPublisher(cfg, logger),
		logger:    logger,
		interval:  interval,
		batchSize: batchSize,
	}
}

func (w *OutboxWorker) Run(ctx context.Context) {
	if w == nil || w.outbox == nil {
		return
	}

	defer func() {
		if err := w.publisher.Close(); err != nil {
			w.logger.Error("close outbox publisher", "error", err)
		}
	}()

	w.processBatch(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("outbox worker stopped")
			return
		case <-ticker.C:
			w.processBatch(ctx)
		}
	}
}

func (w *OutboxWorker) processBatch(ctx context.Context) {
	events, err := w.outbox.FetchUnpublished(ctx, w.batchSize)
	if err != nil {
		w.logger.Error("fetch unpublished outbox events", "error", err)
		return
	}

	for _, event := range events {
		if err := w.publisher.Publish(ctx, event); err != nil {
			w.logger.Error("publish outbox event",
				"event_id", event.ID,
				"event_type", event.EventType,
				"error", err,
			)
			continue
		}

		if err := w.outbox.MarkPublished(ctx, event.ID); err != nil {
			w.logger.Error("mark outbox event published",
				"event_id", event.ID,
				"event_type", event.EventType,
				"error", err,
			)
			continue
		}

		w.logger.Info("outbox event published",
			"event_id", event.ID,
			"event_type", event.EventType,
		)
	}
}

func newOutboxPublisher(cfg OutboxWorkerConfig, logger *slog.Logger) outboxPublisher {
	if len(cfg.Brokers) == 0 || cfg.Topic == "" {
		return &logOutboxPublisher{logger: logger}
	}

	return &kafkaOutboxPublisher{
		topic: cfg.Topic,
		writer: &kafka.Writer{
			Addr:         kafka.TCP(cfg.Brokers...),
			Topic:        cfg.Topic,
			Balancer:     &kafka.LeastBytes{},
			RequiredAcks: kafka.RequireOne,
			Async:        false,
		},
	}
}

type logOutboxPublisher struct {
	logger *slog.Logger
}

func (p *logOutboxPublisher) Publish(ctx context.Context, event *domain.OutboxEvent) error {
	p.logger.InfoContext(ctx, "outbox event handled without kafka",
		"event_id", event.ID,
		"event_type", event.EventType,
	)
	return nil
}

func (p *logOutboxPublisher) Close() error {
	return nil
}

type kafkaOutboxPublisher struct {
	topic  string
	writer *kafka.Writer
}

func (p *kafkaOutboxPublisher) Publish(ctx context.Context, event *domain.OutboxEvent) error {
	if p.writer == nil {
		return fmt.Errorf("kafka writer is not configured")
	}

	return p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(strconv.FormatInt(event.ID, 10)),
		Value: event.Payload,
		Headers: []kafka.Header{
			{Key: "event_type", Value: []byte(event.EventType)},
		},
	})
}

func (p *kafkaOutboxPublisher) Close() error {
	if p.writer == nil {
		return nil
	}
	return p.writer.Close()
}
