package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests by method, path, and status code.",
	}, []string{"method", "path", "status"})

	httpDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency in seconds.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"method", "path"})

	activeRequests = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "http_active_requests",
		Help: "Number of in-flight HTTP requests.",
	})
)

func RecordRequest(method, path string, statusCode int, duration time.Duration) {
	httpRequestsTotal.WithLabelValues(method, path, strconv.Itoa(statusCode)).Inc()
	httpDurationSeconds.WithLabelValues(method, path).Observe(duration.Seconds())
}

func IncrementActive() { activeRequests.Inc() }
func DecrementActive() { activeRequests.Dec() }
