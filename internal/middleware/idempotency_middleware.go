package middleware

import (
	"bytes"
	"net/http"
)

type idempotencyResponseWriter struct {
	http.ResponseWriter
	statusCode int
	buf        bytes.Buffer
}

func newIdempotencyResponseWriter(w http.ResponseWriter) *idempotencyResponseWriter {
	return &idempotencyResponseWriter{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
	}
}

func (w *idempotencyResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *idempotencyResponseWriter) Write(b []byte) (int, error) {
	w.buf.Write(b)
	return w.ResponseWriter.Write(b)
}

func Idempotency(store *IdempotencyStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				key = r.Header.Get("X-Idempotency-Key")
			}
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			if r.Header.Get("X-Idempotency-Key") == "" {
				r.Header.Set("X-Idempotency-Key", key)
			}

			merchant, ok := GetMerchant(r.Context())
			if !ok || merchant == nil {
				next.ServeHTTP(w, r)
				return
			}

			result, err := store.Check(r.Context(), merchant.ID, key)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}

			switch result.Decision {
			case IdempotencyCompleted:
				w.Header().Set("Content-Type", "application/json")
				if result.StatusCode == 0 {
					result.StatusCode = http.StatusOK
				}
				w.WriteHeader(result.StatusCode)
				if len(result.Body) > 0 {
					_, _ = w.Write(result.Body)
				}
				return
			case IdempotencyInProgress:
				writeError(w, http.StatusConflict, "IDEMPOTENCY_IN_PROGRESS", "request with this idempotency key is already in progress")
				return
			}

			wrapped := newIdempotencyResponseWriter(w)
			next.ServeHTTP(wrapped, r)

			if wrapped.statusCode >= 200 && wrapped.statusCode < 300 {
				if err := store.StoreCompleted(r.Context(), merchant.ID, key, wrapped.statusCode, wrapped.buf.Bytes()); err != nil {
					_ = store.Release(r.Context(), merchant.ID, key)
				}
				return
			}

			_ = store.Release(r.Context(), merchant.ID, key)
		})
	}
}
