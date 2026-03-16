package metrics

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

type Metrics struct {
	mu               sync.RWMutex
	requestCount     map[string]int64
	requestDurations map[string][]float64
	activeRequests   int64
	txByStatus       map[string]int64
	startTime        time.Time
}

func New() *Metrics {
	return &Metrics{
		requestCount:     make(map[string]int64),
		requestDurations: make(map[string][]float64),
		txByStatus:       make(map[string]int64),
		startTime:        time.Now(),
	}
}

func (m *Metrics) RecordRequest(method, path string, statusCode int, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	countKey := fmt.Sprintf("%s %s → %d", method, path, statusCode)
	m.requestCount[countKey]++

	latencyKey := fmt.Sprintf("%s %s", method, path)
	m.requestDurations[latencyKey] = append(m.requestDurations[latencyKey], duration.Seconds())
}

func (m *Metrics) RecordTransaction(status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.txByStatus[status]++
}

func (m *Metrics) IncrementActive() {
	m.mu.Lock()
	m.activeRequests++
	m.mu.Unlock()
}

func (m *Metrics) DecrementActive() {
	m.mu.Lock()
	m.activeRequests--
	m.mu.Unlock()
}

type Snapshot struct {
	Uptime            string                  `json:"uptime"`
	ActiveRequests    int64                   `json:"active_requests"`
	RequestCounts     map[string]int64        `json:"request_counts"`
	TransactionCounts map[string]int64        `json:"transaction_counts"`
	Latency           map[string]LatencyStats `json:"latency"`
}

type LatencyStats struct {
	Count int     `json:"count"`
	Avg   float64 `json:"avg_sec"`
	P50   float64 `json:"p50_sec"`
	P95   float64 `json:"p95_sec"`
	P99   float64 `json:"p99_sec"`
	Max   float64 `json:"max_sec"`
}

func (m *Metrics) GetSnapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	reqCounts := make(map[string]int64, len(m.requestCount))
	for k, v := range m.requestCount {
		reqCounts[k] = v
	}

	txCounts := make(map[string]int64, len(m.txByStatus))
	for k, v := range m.txByStatus {
		txCounts[k] = v
	}

	latency := make(map[string]LatencyStats, len(m.requestDurations))
	for path, durations := range m.requestDurations {
		if len(durations) == 0 {
			continue
		}
		sorted := make([]float64, len(durations))
		copy(sorted, durations)
		sort.Float64s(sorted)

		latency[path] = LatencyStats{
			Count: len(sorted),
			Avg:   average(sorted),
			P50:   percentile(sorted, 50),
			P95:   percentile(sorted, 95),
			P99:   percentile(sorted, 99),
			Max:   sorted[len(sorted)-1],
		}
	}

	return Snapshot{
		Uptime:            time.Since(m.startTime).Truncate(time.Second).String(),
		ActiveRequests:    m.activeRequests,
		RequestCounts:     reqCounts,
		TransactionCounts: txCounts,
		Latency:           latency,
	}
}

func percentile(sorted []float64, p int) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	idx := int(float64(n-1) * float64(p) / 100.0)
	return sorted[idx]
}

func average(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}
