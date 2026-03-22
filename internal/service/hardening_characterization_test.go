package service

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aszender/payflow/internal/domain"
	"github.com/aszender/payflow/internal/repository"
	"github.com/aszender/payflow/internal/repository/mock"
)

var errBalanceWriteFailed = errors.New("balance write failed")

type failingBalanceMerchantRepo struct {
	inner *mock.MerchantRepo
	err   error
}

func newFailingBalanceMerchantRepo(err error) *failingBalanceMerchantRepo {
	return &failingBalanceMerchantRepo{
		inner: mock.NewMerchantRepo(),
		err:   err,
	}
}

func (r *failingBalanceMerchantRepo) WithTx(_ *sql.Tx) repository.MerchantRepository {
	return r
}

func (r *failingBalanceMerchantRepo) GetByID(ctx context.Context, id string) (*domain.Merchant, error) {
	return r.inner.GetByID(ctx, id)
}

func (r *failingBalanceMerchantRepo) GetByAPIKey(ctx context.Context, apiKey string) (*domain.Merchant, error) {
	return r.inner.GetByAPIKey(ctx, apiKey)
}

func (r *failingBalanceMerchantRepo) UpdateBalance(ctx context.Context, id string, delta int64) error {
	if delta > 0 {
		return r.err
	}
	return r.inner.UpdateBalance(ctx, id, delta)
}

type multiWorkerGatePublisher struct {
	mu             sync.Mutex
	publishCount   map[int64]int
	started        chan struct{}
	releasePublish chan struct{}
}

func newMultiWorkerGatePublisher() *multiWorkerGatePublisher {
	return &multiWorkerGatePublisher{
		publishCount:   make(map[int64]int),
		started:        make(chan struct{}, 2),
		releasePublish: make(chan struct{}),
	}
}

func (p *multiWorkerGatePublisher) Publish(ctx context.Context, event *domain.OutboxEvent) error {
	p.mu.Lock()
	p.publishCount[event.ID]++
	p.mu.Unlock()

	select {
	case p.started <- struct{}{}:
	default:
	}

	select {
	case <-p.releasePublish:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *multiWorkerGatePublisher) Close() error {
	return nil
}

func (p *multiWorkerGatePublisher) countFor(id int64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.publishCount[id]
}

func waitForNSignals(t *testing.T, ch <-chan struct{}, count int, timeout time.Duration) {
	t.Helper()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for i := 0; i < count; i++ {
		select {
		case <-ch:
		case <-timer.C:
			t.Fatalf("timed out waiting for %d signals", count)
		}
	}
}

func TestCreateTransaction_CompletionFailureLeavesTransactionUncompleted(t *testing.T) {
	merchants := newFailingBalanceMerchantRepo(errBalanceWriteFailed)
	txns := mock.NewTransactionRepo()
	events := mock.NewEventRepo()
	outbox := mock.NewOutboxRepo()

	svc := NewPaymentService(PaymentServiceConfig{
		Merchants:      merchants,
		Transactions:   txns,
		Events:         events,
		Outbox:         outbox,
		Logger:         newTestLogger(),
		BankClient:     &SimulatedBankClient{Latency: time.Millisecond},
		CircuitBreaker: NewCircuitBreaker(3, 50*time.Millisecond),
		RetryConfig: RetryConfig{
			MaxAttempts: 1,
			BaseDelay:   time.Millisecond,
			MaxDelay:    time.Millisecond,
			Jitter:      0,
			ShouldRetry: isRetryableBankError,
		},
		BankTimeout:         time.Second,
		MaxTransactionCents: 2500000,
	})

	tx, err := svc.CreateTransaction(context.Background(), CreateTransactionInput{
		MerchantID:  "m_001",
		AmountCents: 12500,
		Currency:    "CAD",
	})
	if !errors.Is(err, errBalanceWriteFailed) {
		t.Fatalf("expected balance write failure, got %v", err)
	}
	if tx == nil {
		t.Fatal("expected transaction to be returned on completion failure")
	}
	if tx.Status != domain.TxStatusProcessing {
		t.Fatalf("expected in-memory transaction status PROCESSING, got %s", tx.Status)
	}

	stored, err := txns.GetByID(context.Background(), tx.ID)
	if err != nil {
		t.Fatalf("load stored transaction: %v", err)
	}
	if stored.Status != domain.TxStatusProcessing {
		t.Fatalf("expected persisted status PROCESSING, got %s", stored.Status)
	}

	merchant, err := merchants.GetByID(context.Background(), "m_001")
	if err != nil {
		t.Fatalf("load merchant: %v", err)
	}
	if merchant.BalanceCents != 0 {
		t.Fatalf("expected unchanged merchant balance after failed credit, got %d", merchant.BalanceCents)
	}

	history, err := events.ListByTransaction(context.Background(), tx.ID)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected only created+processing audit events, got %d", len(history))
	}
	if got := history[len(history)-1].ToStatus; got != string(domain.TxStatusProcessing) {
		t.Fatalf("expected final audit status PROCESSING, got %s", got)
	}

	if got := len(outbox.Events); got != 1 {
		t.Fatalf("expected only the created outbox event, got %d", got)
	}
	if outbox.Events[0].EventType != "transaction.created" {
		t.Fatalf("expected created outbox event, got %s", outbox.Events[0].EventType)
	}
}

type claimingOutboxRepo struct {
	mu           sync.Mutex
	event        *domain.OutboxEvent
	claimed      bool
	markAttempts int
}

func newClaimingOutboxRepo(event *domain.OutboxEvent) *claimingOutboxRepo {
	eventCopy := *event
	return &claimingOutboxRepo{event: &eventCopy}
}

func (r *claimingOutboxRepo) Create(_ context.Context, _ *domain.OutboxEvent) error {
	return nil
}

func (r *claimingOutboxRepo) FetchUnpublished(_ context.Context, limit int) ([]*domain.OutboxEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if limit <= 0 || r.event == nil || r.event.Published || r.claimed {
		return nil, nil
	}

	r.claimed = true
	eventCopy := *r.event
	return []*domain.OutboxEvent{&eventCopy}, nil
}

func (r *claimingOutboxRepo) MarkPublished(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.markAttempts++
	if r.event != nil && r.event.ID == id {
		r.event.Published = true
	}
	return nil
}

func (r *claimingOutboxRepo) WithTx(_ *sql.Tx) repository.OutboxRepository {
	return r
}

func TestOutboxWorker_ClaimedEventsPublishOnlyOnceAcrossWorkers(t *testing.T) {
	repo := newClaimingOutboxRepo(newOutboxEvent(1, "transaction.completed"))
	publisher := newMultiWorkerGatePublisher()

	workerA := &OutboxWorker{
		outbox:      repo,
		publisher:   publisher,
		logger:      newTestLogger(),
		batchSize:   1,
		concurrency: 1,
	}
	workerB := &OutboxWorker{
		outbox:      repo,
		publisher:   publisher,
		logger:      newTestLogger(),
		batchSize:   1,
		concurrency: 1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		workerA.processBatch(ctx)
	}()
	go func() {
		defer wg.Done()
		workerB.processBatch(ctx)
	}()

	waitForNSignals(t, publisher.started, 1, time.Second)
	close(publisher.releasePublish)
	wg.Wait()

	if got := publisher.countFor(1); got != 1 {
		t.Fatalf("expected a single publish for event 1, got %d", got)
	}
	if got := repo.markAttempts; got != 1 {
		t.Fatalf("expected a single mark attempt, got %d", got)
	}
	if repo.event == nil || !repo.event.Published {
		t.Fatal("expected event to end published after duplicate processing")
	}
}
