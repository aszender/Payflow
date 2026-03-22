package service

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aszender/payflow/internal/domain"
	"github.com/aszender/payflow/internal/repository"
)

type testOutboxRepo struct {
	mu           sync.Mutex
	events       []*domain.OutboxEvent
	fetchErr     error
	markErrs     map[int64]error
	marked       []int64
	markAttempts []int64
	fetchCalls   int
}

func (r *testOutboxRepo) Create(context.Context, *domain.OutboxEvent) error {
	return nil
}

func (r *testOutboxRepo) FetchUnpublished(ctx context.Context, limit int) ([]*domain.OutboxEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.fetchErr != nil {
		return nil, r.fetchErr
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.fetchCalls++
	if limit > len(r.events) {
		limit = len(r.events)
	}

	events := make([]*domain.OutboxEvent, 0, limit)
	for i := 0; i < limit; i++ {
		eventCopy := *r.events[i]
		events = append(events, &eventCopy)
	}
	return events, nil
}

func (r *testOutboxRepo) MarkPublished(ctx context.Context, id int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.markAttempts = append(r.markAttempts, id)
	if err := r.markErrs[id]; err != nil {
		return err
	}

	r.marked = append(r.marked, id)
	return nil
}

func (r *testOutboxRepo) WithTx(*sql.Tx) repository.OutboxRepository {
	return r
}

type testPublisher struct {
	publish func(context.Context, *domain.OutboxEvent) error
}

func (p *testPublisher) Publish(ctx context.Context, event *domain.OutboxEvent) error {
	if p.publish == nil {
		return nil
	}
	return p.publish(ctx, event)
}

func (p *testPublisher) Close() error {
	return nil
}

func TestOutboxWorkerProcessBatchMarksPublishedAfterSuccessfulPublish(t *testing.T) {
	repo := &testOutboxRepo{
		events: []*domain.OutboxEvent{
			{ID: 1, EventType: "transaction.created"},
			{ID: 2, EventType: "transaction.completed"},
			{ID: 3, EventType: "transaction.refunded"},
		},
	}

	worker := &OutboxWorker{
		outbox:      repo,
		publisher:   &testPublisher{},
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		batchSize:   10,
		concurrency: 2,
	}

	worker.processBatch(context.Background())

	if got, want := repo.fetchCalls, 1; got != want {
		t.Fatalf("expected %d fetch call, got %d", want, got)
	}

	assertIDs(t, repo.marked, []int64{1, 2, 3})
	assertIDs(t, repo.markAttempts, []int64{1, 2, 3})
}

func TestOutboxWorkerProcessBatchLeavesFailuresUnpublished(t *testing.T) {
	repo := &testOutboxRepo{
		events: []*domain.OutboxEvent{
			{ID: 1, EventType: "transaction.created"},
			{ID: 2, EventType: "transaction.completed"},
			{ID: 3, EventType: "transaction.refunded"},
		},
		markErrs: map[int64]error{
			3: errors.New("mark failed"),
		},
	}

	worker := &OutboxWorker{
		outbox: repo,
		publisher: &testPublisher{
			publish: func(_ context.Context, event *domain.OutboxEvent) error {
				if event.ID == 2 {
					return errors.New("publish failed")
				}
				return nil
			},
		},
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		batchSize:   10,
		concurrency: 3,
	}

	worker.processBatch(context.Background())

	assertIDs(t, repo.marked, []int64{1})
	assertIDs(t, repo.markAttempts, []int64{1, 3})
}

func TestOutboxWorkerProcessBatchRespectsContextCancellation(t *testing.T) {
	repo := &testOutboxRepo{
		events: []*domain.OutboxEvent{
			{ID: 1, EventType: "transaction.created"},
			{ID: 2, EventType: "transaction.completed"},
		},
	}

	started := make(chan struct{}, len(repo.events))
	worker := &OutboxWorker{
		outbox: repo,
		publisher: &testPublisher{
			publish: func(ctx context.Context, _ *domain.OutboxEvent) error {
				started <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			},
		},
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		batchSize:   10,
		concurrency: 2,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		worker.processBatch(ctx)
		close(done)
	}()

	waitForStarted(t, started, 1)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("processBatch did not return after context cancellation")
	}

	if len(repo.markAttempts) != 0 {
		t.Fatalf("expected no mark attempts after cancellation, got %v", repo.markAttempts)
	}
}

func TestOutboxWorkerProcessBatchBoundsConcurrency(t *testing.T) {
	repo := &testOutboxRepo{
		events: []*domain.OutboxEvent{
			{ID: 1, EventType: "transaction.created"},
			{ID: 2, EventType: "transaction.created"},
			{ID: 3, EventType: "transaction.created"},
			{ID: 4, EventType: "transaction.created"},
			{ID: 5, EventType: "transaction.created"},
		},
	}

	release := make(chan struct{})
	started := make(chan struct{}, len(repo.events))
	var active atomic.Int32
	var maxActive atomic.Int32

	worker := &OutboxWorker{
		outbox: repo,
		publisher: &testPublisher{
			publish: func(ctx context.Context, _ *domain.OutboxEvent) error {
				current := active.Add(1)
				updateMax(&maxActive, current)
				started <- struct{}{}
				defer active.Add(-1)

				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		},
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		batchSize:   10,
		concurrency: 2,
	}

	done := make(chan struct{})
	go func() {
		worker.processBatch(context.Background())
		close(done)
	}()

	waitForStarted(t, started, 2)
	time.Sleep(50 * time.Millisecond)
	if got := maxActive.Load(); got != 2 {
		t.Fatalf("expected blocked publishes to cap at 2 workers, got %d", got)
	}

	close(release)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("processBatch did not finish after releasing blocked publishers")
	}

	if got := maxActive.Load(); got != 2 {
		t.Fatalf("expected max concurrency 2, got %d", got)
	}

	assertIDs(t, repo.marked, []int64{1, 2, 3, 4, 5})
}

func waitForStarted(t *testing.T, started <-chan struct{}, count int) {
	t.Helper()

	deadline := time.After(time.Second)
	for i := 0; i < count; i++ {
		select {
		case <-started:
		case <-deadline:
			t.Fatalf("timed out waiting for %d worker starts", count)
		}
	}
}

func updateMax(dst *atomic.Int32, current int32) {
	for {
		prev := dst.Load()
		if current <= prev {
			return
		}
		if dst.CompareAndSwap(prev, current) {
			return
		}
	}
}

func assertIDs(t *testing.T, got, want []int64) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}

	counts := make(map[int64]int, len(want))
	for _, id := range want {
		counts[id]++
	}
	for _, id := range got {
		counts[id]--
	}
	for _, count := range counts {
		if count != 0 {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}
