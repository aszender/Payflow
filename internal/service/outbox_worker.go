package service

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/aszender/payflow/internal/concurrency"
	"github.com/aszender/payflow/internal/domain"
	"github.com/aszender/payflow/internal/repository"
)

const defaultOutboxConcurrency = 4

type OutboxWorkerConfig struct {
	Outbox      repository.OutboxRepository
	Brokers     []string
	Topic       string
	Logger      *slog.Logger
	Interval    time.Duration
	BatchSize   int
	Concurrency int
}

type outboxPublisher interface {
	Publish(context.Context, *domain.OutboxEvent) error
	Close() error
}

type OutboxWorker struct {
	outbox      repository.OutboxRepository
	publisher   outboxPublisher
	logger      *slog.Logger
	interval    time.Duration
	batchSize   int
	concurrency int
}

type outboxProcessResult struct {
	eventID   int64
	eventType string
	stage     string
	err       error
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

	concurrencyLimit := cfg.Concurrency
	if concurrencyLimit <= 0 {
		concurrencyLimit = defaultOutboxConcurrency
	}
	if concurrencyLimit > batchSize {
		concurrencyLimit = batchSize
	}

	return &OutboxWorker{
		outbox:      cfg.Outbox,
		publisher:   newOutboxPublisher(cfg, logger),
		logger:      logger,
		interval:    interval,
		batchSize:   batchSize,
		concurrency: concurrencyLimit,
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
	if err := ctx.Err(); err != nil {
		return
	}

	events, err := w.outbox.FetchUnpublished(ctx, w.batchSize)
	if err != nil {
		w.logger.Error("fetch unpublished outbox events", "error", err)
		return
	}
	if len(events) == 0 {
		return
	}

	for _, result := range w.processEvents(ctx, events) {
		w.logProcessResult(result)
	}
}

func (w *OutboxWorker) processEvents(ctx context.Context, events []*domain.OutboxEvent) []outboxProcessResult {
	workerCount := w.workerCount(len(events))
	pool := concurrency.NewWorkerPool(workerCount, len(events))

	// Buffer all results so workers can finish and exit even if collection happens after shutdown.
	resultsCh := make(chan outboxProcessResult, len(events))
	submitted := 0

	for _, event := range events {
		if ctx.Err() != nil {
			break
		}

		current := event
		if err := pool.Submit(func() {
			resultsCh <- w.processEvent(ctx, current)
		}); err != nil {
			resultsCh <- outboxProcessResult{
				eventID:   current.ID,
				eventType: current.EventType,
				stage:     "submit",
				err:       err,
			}
			continue
		}
		submitted++
	}

	pool.Shutdown()
	close(resultsCh)

	results := make([]outboxProcessResult, 0, submitted)
	for result := range resultsCh {
		results = append(results, result)
	}

	return results
}

func (w *OutboxWorker) processEvent(ctx context.Context, event *domain.OutboxEvent) outboxProcessResult {
	result := outboxProcessResult{
		eventID:   event.ID,
		eventType: event.EventType,
		stage:     "publish",
	}

	if err := ctx.Err(); err != nil {
		result.err = err
		return result
	}

	if err := w.publisher.Publish(ctx, event); err != nil {
		result.err = err
		return result
	}

	result.stage = "mark_published"
	if err := w.outbox.MarkPublished(ctx, event.ID); err != nil {
		result.err = err
		return result
	}

	result.stage = "completed"
	return result
}

func (w *OutboxWorker) workerCount(batchLen int) int {
	if batchLen <= 0 {
		return 1
	}
	if w.concurrency <= 0 {
		return 1
	}
	if w.concurrency > batchLen {
		return batchLen
	}
	return w.concurrency
}

func (w *OutboxWorker) logProcessResult(result outboxProcessResult) {
	if result.err != nil {
		w.logger.Error("outbox event processing failed",
			"event_id", result.eventID,
			"event_type", result.eventType,
			"stage", result.stage,
			"error", result.err,
		)
		return
	}

	w.logger.Info("outbox event published",
		"event_id", result.eventID,
		"event_type", result.eventType,
	)
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
