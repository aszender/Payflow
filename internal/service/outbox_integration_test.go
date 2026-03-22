package service

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aszender/payflow/internal/domain"
	"github.com/aszender/payflow/internal/repository"
	"github.com/aszender/payflow/internal/repository/mock"
)

var errTestMarkPublished = errors.New("mark published failed")

type reliabilityOutboxRepo struct {
	mu                    sync.RWMutex
	events                map[int64]*domain.OutboxEvent
	order                 []int64
	nextID                int64
	markCalls             map[int64]int
	markFailuresRemaining map[int64]int
	markErrByID           map[int64]error
}

func newReliabilityOutboxRepo(events ...*domain.OutboxEvent) *reliabilityOutboxRepo {
	repo := &reliabilityOutboxRepo{
		events:                make(map[int64]*domain.OutboxEvent),
		nextID:                1,
		markCalls:             make(map[int64]int),
		markFailuresRemaining: make(map[int64]int),
		markErrByID:           make(map[int64]error),
	}

	for _, event := range events {
		_ = repo.Create(context.Background(), event)
	}

	return repo
}

func (r *reliabilityOutboxRepo) WithTx(_ *sql.Tx) repository.OutboxRepository {
	return r
}

func (r *reliabilityOutboxRepo) Create(_ context.Context, event *domain.OutboxEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	copy := *event
	if copy.ID == 0 {
		copy.ID = r.nextID
		r.nextID++
	} else if copy.ID >= r.nextID {
		r.nextID = copy.ID + 1
	}

	r.events[copy.ID] = &copy
	r.order = append(r.order, copy.ID)
	return nil
}

func (r *reliabilityOutboxRepo) FetchUnpublished(_ context.Context, limit int) ([]*domain.OutboxEvent, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*domain.OutboxEvent
	for _, id := range r.order {
		event := r.events[id]
		if event == nil || event.Published {
			continue
		}

		copy := *event
		result = append(result, &copy)
		if len(result) >= limit {
			break
		}
	}

	return result, nil
}

func (r *reliabilityOutboxRepo) MarkPublished(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.markCalls[id]++
	if r.markFailuresRemaining[id] > 0 {
		r.markFailuresRemaining[id]--
		if err := r.markErrByID[id]; err != nil {
			return err
		}
		return errTestMarkPublished
	}

	event := r.events[id]
	if event == nil {
		return nil
	}
	event.Published = true
	return nil
}

func (r *reliabilityOutboxRepo) setMarkFailure(id int64, attempts int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.markFailuresRemaining[id] = attempts
	r.markErrByID[id] = err
}

func (r *reliabilityOutboxRepo) isPublished(id int64) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	event := r.events[id]
	return event != nil && event.Published
}

func (r *reliabilityOutboxRepo) publishedCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	count := 0
	for _, event := range r.events {
		if event.Published {
			count++
		}
	}
	return count
}

func (r *reliabilityOutboxRepo) totalCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.events)
}

func (r *reliabilityOutboxRepo) markCount(id int64) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.markCalls[id]
}

type reliabilityPublisher struct {
	mu            sync.Mutex
	attempts      map[int64]int
	successes     map[int64]int
	failRemaining map[int64]int
	failErr       error
	blockEventID  int64
	blockStarted  chan struct{}
	closeCalls    int
}

func newReliabilityPublisher() *reliabilityPublisher {
	return &reliabilityPublisher{
		attempts:      make(map[int64]int),
		successes:     make(map[int64]int),
		failRemaining: make(map[int64]int),
		failErr:       errors.New("publish failed"),
	}
}

func (p *reliabilityPublisher) Publish(ctx context.Context, event *domain.OutboxEvent) error {
	var shouldBlock bool

	p.mu.Lock()
	p.attempts[event.ID]++
	if event.ID == p.blockEventID && p.blockStarted != nil {
		close(p.blockStarted)
		p.blockStarted = nil
		shouldBlock = true
	}
	if p.failRemaining[event.ID] > 0 {
		p.failRemaining[event.ID]--
		err := p.failErr
		p.mu.Unlock()
		return err
	}
	if !shouldBlock {
		p.successes[event.ID]++
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	<-ctx.Done()
	return ctx.Err()
}

func (p *reliabilityPublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeCalls++
	return nil
}

func (p *reliabilityPublisher) attemptCount(id int64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts[id]
}

func (p *reliabilityPublisher) totalSuccesses() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	total := 0
	for _, count := range p.successes {
		total += count
	}
	return total
}

func (p *reliabilityPublisher) closeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeCalls
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newOutboxEvent(id int64, eventType string) *domain.OutboxEvent {
	return &domain.OutboxEvent{
		ID:        id,
		EventType: eventType,
		Payload:   []byte(`{"transaction_id":"tx_test","status":"PENDING"}`),
		CreatedAt: time.Now(),
	}
}

func startWorker(ctx context.Context, worker *OutboxWorker) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()
	return done
}

func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("condition not met within %s", timeout)
}

func waitForDone(t *testing.T, done <-chan struct{}, timeout time.Duration) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("worker did not stop within %s", timeout)
	}
}

func TestOutboxWorkerRun_PublishesAndMarksPendingEvents(t *testing.T) {
	repo := newReliabilityOutboxRepo(
		newOutboxEvent(1, "transaction.created"),
		newOutboxEvent(2, "transaction.updated"),
		newOutboxEvent(3, "transaction.refunded"),
	)
	publisher := newReliabilityPublisher()
	worker := &OutboxWorker{
		outbox:    repo,
		publisher: publisher,
		logger:    newTestLogger(),
		interval:  5 * time.Millisecond,
		batchSize: 10,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startWorker(ctx, worker)

	waitFor(t, time.Second, func() bool {
		return repo.publishedCount() == 3
	})

	cancel()
	waitForDone(t, done, time.Second)

	for _, id := range []int64{1, 2, 3} {
		if !repo.isPublished(id) {
			t.Fatalf("event %d was not marked published", id)
		}
		if got := publisher.attemptCount(id); got != 1 {
			t.Fatalf("expected one publish attempt for event %d, got %d", id, got)
		}
	}
	if got := publisher.totalSuccesses(); got != 3 {
		t.Fatalf("expected 3 successful publishes, got %d", got)
	}
	if got := publisher.closeCount(); got != 1 {
		t.Fatalf("expected publisher close once, got %d", got)
	}
}

func TestOutboxWorkerRun_RetriesAfterTransientPublishFailure(t *testing.T) {
	repo := newReliabilityOutboxRepo(newOutboxEvent(1, "transaction.created"))
	publisher := newReliabilityPublisher()
	publisher.failRemaining[1] = 1

	worker := &OutboxWorker{
		outbox:    repo,
		publisher: publisher,
		logger:    newTestLogger(),
		interval:  5 * time.Millisecond,
		batchSize: 10,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startWorker(ctx, worker)

	waitFor(t, time.Second, func() bool {
		return repo.isPublished(1)
	})

	cancel()
	waitForDone(t, done, time.Second)

	if got := publisher.attemptCount(1); got != 2 {
		t.Fatalf("expected 2 publish attempts, got %d", got)
	}
	if got := repo.markCount(1); got != 1 {
		t.Fatalf("expected mark published once after retry, got %d", got)
	}
}

func TestOutboxWorkerRun_RetriesWhenMarkPublishedFails(t *testing.T) {
	repo := newReliabilityOutboxRepo(newOutboxEvent(1, "transaction.created"))
	repo.setMarkFailure(1, 1, errTestMarkPublished)
	publisher := newReliabilityPublisher()

	worker := &OutboxWorker{
		outbox:    repo,
		publisher: publisher,
		logger:    newTestLogger(),
		interval:  5 * time.Millisecond,
		batchSize: 10,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startWorker(ctx, worker)

	waitFor(t, time.Second, func() bool {
		return repo.isPublished(1)
	})

	cancel()
	waitForDone(t, done, time.Second)

	if got := publisher.attemptCount(1); got != 2 {
		t.Fatalf("expected duplicate publish after mark failure, got %d attempts", got)
	}
	if got := repo.markCount(1); got != 2 {
		t.Fatalf("expected 2 mark attempts, got %d", got)
	}
}

func TestOutboxWorkerRun_StopsOnContextCancellation(t *testing.T) {
	repo := newReliabilityOutboxRepo(newOutboxEvent(1, "transaction.created"))
	publisher := newReliabilityPublisher()
	publisher.blockEventID = 1
	blockStarted := make(chan struct{})
	publisher.blockStarted = blockStarted

	worker := &OutboxWorker{
		outbox:    repo,
		publisher: publisher,
		logger:    newTestLogger(),
		interval:  50 * time.Millisecond,
		batchSize: 10,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startWorker(ctx, worker)

	select {
	case <-blockStarted:
	case <-time.After(time.Second):
		t.Fatal("publish did not start")
	}

	cancel()
	waitForDone(t, done, time.Second)

	if repo.isPublished(1) {
		t.Fatal("event should remain unpublished after cancellation")
	}
	if got := publisher.attemptCount(1); got != 1 {
		t.Fatalf("expected one blocked publish attempt, got %d", got)
	}
	if got := publisher.closeCount(); got != 1 {
		t.Fatalf("expected publisher close once, got %d", got)
	}
}

func TestOutboxWorkerRun_HandlesConcurrentCreatesUnderLoad(t *testing.T) {
	repo := newReliabilityOutboxRepo()
	publisher := newReliabilityPublisher()
	worker := &OutboxWorker{
		outbox:    repo,
		publisher: publisher,
		logger:    newTestLogger(),
		interval:  5 * time.Millisecond,
		batchSize: 64,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startWorker(ctx, worker)

	const producers = 12
	const eventsPerProducer = 10
	const totalEvents = producers * eventsPerProducer

	var wg sync.WaitGroup
	for producer := 0; producer < producers; producer++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < eventsPerProducer; i++ {
				if err := repo.Create(context.Background(), newOutboxEvent(0, "transaction.created")); err != nil {
					t.Errorf("create event: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	waitFor(t, 2*time.Second, func() bool {
		return repo.totalCount() == totalEvents && repo.publishedCount() == totalEvents
	})

	cancel()
	waitForDone(t, done, time.Second)

	if got := publisher.totalSuccesses(); got != totalEvents {
		t.Fatalf("expected %d successful publishes, got %d", totalEvents, got)
	}
}

func TestCreateTransaction_BankTimeoutLeavesBalanceUnchangedAndMarksFailure(t *testing.T) {
	merchants := mock.NewMerchantRepo()
	txns := mock.NewTransactionRepo()
	events := mock.NewEventRepo()
	outbox := mock.NewOutboxRepo()

	svc := NewPaymentService(PaymentServiceConfig{
		Merchants:    merchants,
		Transactions: txns,
		Events:       events,
		Outbox:       outbox,
		Logger:       newTestLogger(),
		BankClient: &SimulatedBankClient{
			Latency: 50 * time.Millisecond,
		},
		CircuitBreaker: NewCircuitBreaker(3, 50*time.Millisecond),
		RetryConfig: RetryConfig{
			MaxAttempts: 1,
			BaseDelay:   time.Millisecond,
			MaxDelay:    time.Millisecond,
			Jitter:      0,
			ShouldRetry: isRetryableBankError,
		},
		BankTimeout:    5 * time.Millisecond,
		MaxTransaction: 25000,
	})

	tx, err := svc.CreateTransaction(context.Background(), CreateTransactionInput{
		MerchantID: "m_001",
		Amount:     75,
		Currency:   "CAD",
	})
	if !errors.Is(err, domain.ErrBankTimeout) {
		t.Fatalf("expected bank timeout error, got %v", err)
	}
	if tx == nil {
		t.Fatal("expected transaction to be returned with timeout error")
	}
	if tx.Status != domain.TxStatusFailed {
		t.Fatalf("expected failed transaction after bank timeout, got %s", tx.Status)
	}

	stored, err := txns.GetByID(context.Background(), tx.ID)
	if err != nil {
		t.Fatalf("load stored transaction: %v", err)
	}
	if stored.Status != domain.TxStatusFailed {
		t.Fatalf("expected persisted failed status, got %s", stored.Status)
	}

	merchant, err := merchants.GetByID(context.Background(), "m_001")
	if err != nil {
		t.Fatalf("load merchant: %v", err)
	}
	if merchant.Balance != 0 {
		t.Fatalf("expected unchanged balance after bank timeout, got %.2f", merchant.Balance)
	}

	history, err := events.ListByTransaction(context.Background(), tx.ID)
	if err != nil {
		t.Fatalf("load transaction history: %v", err)
	}
	if len(history) < 3 {
		t.Fatalf("expected at least 3 history events, got %d", len(history))
	}
	if history[len(history)-1].ToStatus != string(domain.TxStatusFailed) {
		t.Fatalf("expected final history status FAILED, got %s", history[len(history)-1].ToStatus)
	}
}
