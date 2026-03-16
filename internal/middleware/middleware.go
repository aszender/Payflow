package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/aszender/payflow/internal/metrics"
	"github.com/aszender/payflow/internal/repository"
)

type contextKey string

const (
	RequestIDKey contextKey = "request_id"
	MerchantKey  contextKey = "merchant"
)

type responseWriter struct {
	http.ResponseWriter
	statusCode int
	written    int
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.written += n
	return n, err
}

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = fmt.Sprintf("req_%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Request-ID", requestID)
		ctx := context.WithValue(r.Context(), RequestIDKey, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func GetRequestID(ctx context.Context) string {
	if id, ok := ctx.Value(RequestIDKey).(string); ok {
		return id
	}
	return "unknown"
}

func Logging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			wrapped := newResponseWriter(w)

			next.ServeHTTP(wrapped, r)

			duration := time.Since(start)
			level := slog.LevelInfo
			if wrapped.statusCode >= 500 {
				level = slog.LevelError
			} else if wrapped.statusCode >= 400 {
				level = slog.LevelWarn
			}

			logger.Log(r.Context(), level, "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", wrapped.statusCode,
				"duration_ms", duration.Milliseconds(),
				"bytes", wrapped.written,
				"request_id", GetRequestID(r.Context()),
				"ip", r.RemoteAddr,
			)
		})
	}
}

func MetricsMiddleware(m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.IncrementActive()
			defer m.DecrementActive()

			start := time.Now()
			wrapped := newResponseWriter(w)

			next.ServeHTTP(wrapped, r)

			m.RecordRequest(r.Method, r.URL.Path, wrapped.statusCode, time.Since(start))
		})
	}
}

func APIKeyAuth(merchants repository.MerchantRepository, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing Authorization header")
				return
			}

			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || parts[0] != "Bearer" {
				writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "format: Bearer <api_key>")
				return
			}

			merchant, err := merchants.GetByAPIKey(r.Context(), parts[1])
			if err != nil {
				logger.Warn("auth failed",
					"error", err,
					"request_id", GetRequestID(r.Context()),
					"ip", r.RemoteAddr,
				)
				writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid API key")
				return
			}

			if !merchant.IsActive() {
				writeError(w, http.StatusForbidden, "FORBIDDEN", "merchant account is not active")
				return
			}

			ctx := context.WithValue(r.Context(), MerchantKey, merchant)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

type RateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	rate     int
	burst    int
	interval time.Duration
}

type bucket struct {
	tokens    float64
	lastCheck time.Time
}

func NewRateLimiter(rps, burst int) *RateLimiter {
	return &RateLimiter{
		buckets:  make(map[string]*bucket),
		rate:     rps,
		burst:    burst,
		interval: time.Second,
	}
}

func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, exists := rl.buckets[key]
	if !exists {
		b = &bucket{tokens: float64(rl.burst), lastCheck: now}
		rl.buckets[key] = b
	}

	elapsed := now.Sub(b.lastCheck).Seconds()
	b.tokens += elapsed * float64(rl.rate)
	if b.tokens > float64(rl.burst) {
		b.tokens = float64(rl.burst)
	}
	b.lastCheck = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func RateLimit(limiter *RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiter.Allow(r.RemoteAddr) {
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func Recovery(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					logger.Error("panic recovered",
						"error", err,
						"stack", string(debug.Stack()),
						"request_id", GetRequestID(r.Context()),
						"path", r.URL.Path,
					)
					writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an unexpected error occurred")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID, X-Idempotency-Key")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"success":false,"error":{"code":"%s","message":"%s"}}`, code, message)
}
