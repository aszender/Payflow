package handler

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aszender/payflow/internal/metrics"
	"github.com/aszender/payflow/internal/middleware"
	"github.com/aszender/payflow/internal/repository/mock"
	"github.com/aszender/payflow/internal/service"
)

type handlerTestSuite struct {
	router *chi.Mux
}

func newHandlerTestSuite() *handlerTestSuite {
	logger := slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), &slog.HandlerOptions{Level: slog.LevelError}))
	appMetrics := metrics.New()
	merchantRepo := mock.NewMerchantRepo()

	svc := service.NewPaymentService(service.PaymentServiceConfig{
		Merchants:      merchantRepo,
		Transactions:   mock.NewTransactionRepo(),
		Events:         mock.NewEventRepo(),
		Outbox:         mock.NewOutboxRepo(),
		Logger:         logger,
		BankClient:     &service.SimulatedBankClient{Latency: time.Millisecond},
		CircuitBreaker: service.NewCircuitBreaker(3, 20*time.Millisecond),
		RetryConfig: service.RetryConfig{
			MaxAttempts: 2,
			BaseDelay:   time.Millisecond,
			MaxDelay:    2 * time.Millisecond,
			Jitter:      0,
		},
		BankTimeout:    time.Second,
		MaxTransaction: 25000,
	})

	txHandler := NewTransactionHandler(svc)
	merchantHandler := NewMerchantHandler(svc)
	healthHandler := NewHealthHandler(nil, "test")
	metricsHandler := NewMetricsHandler(appMetrics)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recovery(logger))
	r.Use(middleware.Logging(logger))
	r.Use(middleware.MetricsMiddleware(appMetrics))
	r.Use(middleware.CORS)
	r.Use(middleware.RateLimit(middleware.NewRateLimiter(100, 100)))

	r.Get("/health", healthHandler.Check)
	r.Get("/metrics", metricsHandler.Get)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(middleware.APIKeyAuth(merchantRepo, logger))

		r.Route("/transactions", func(r chi.Router) {
			r.Post("/", txHandler.Create)
			r.Get("/{id}", txHandler.GetByID)
			r.Post("/{id}/refund", txHandler.Refund)
			r.Get("/{id}/events", txHandler.GetEvents)
		})

		r.Route("/merchants", func(r chi.Router) {
			r.Get("/{id}/balance", merchantHandler.GetBalance)
			r.Get("/{id}/transactions", txHandler.ListByMerchant)
		})
	})

	return &handlerTestSuite{router: r}
}

func authRequest(method, path string, body []byte) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk_live_maple_001")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestHealth_ReturnsOK(t *testing.T) {
	suite := newHandlerTestSuite()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	suite.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestCreate_Success(t *testing.T) {
	suite := newHandlerTestSuite()
	req := authRequest(http.MethodPost, "/api/v1/transactions", []byte(`{"merchant_id":"m_001","amount":150,"currency":"CAD"}`))
	req.Header.Set("X-Idempotency-Key", "order_123")
	rec := httptest.NewRecorder()

	suite.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp APIResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Fatal("expected success=true")
	}
}

func TestCreate_InvalidJSON(t *testing.T) {
	suite := newHandlerTestSuite()
	req := authRequest(http.MethodPost, "/api/v1/transactions", []byte(`not-json`))
	rec := httptest.NewRecorder()

	suite.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestCreate_MissingAuth(t *testing.T) {
	suite := newHandlerTestSuite()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/transactions", bytes.NewReader([]byte(`{"merchant_id":"m_001","amount":150,"currency":"CAD"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	suite.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestGetTransaction_Success(t *testing.T) {
	suite := newHandlerTestSuite()
	createReq := authRequest(http.MethodPost, "/api/v1/transactions", []byte(`{"merchant_id":"m_001","amount":125,"currency":"CAD"}`))
	createRec := httptest.NewRecorder()
	suite.router.ServeHTTP(createRec, createReq)

	var createResp struct {
		Success bool `json:"success"`
		Data    struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(createRec.Body).Decode(&createResp); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	req := authRequest(http.MethodGet, "/api/v1/transactions/"+createResp.Data.ID, nil)
	rec := httptest.NewRecorder()
	suite.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestRefund_Success(t *testing.T) {
	suite := newHandlerTestSuite()
	createReq := authRequest(http.MethodPost, "/api/v1/transactions", []byte(`{"merchant_id":"m_001","amount":200,"currency":"CAD"}`))
	createRec := httptest.NewRecorder()
	suite.router.ServeHTTP(createRec, createReq)

	var createResp struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(createRec.Body).Decode(&createResp); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	refundReq := authRequest(http.MethodPost, "/api/v1/transactions/"+createResp.Data.ID+"/refund", []byte(`{"reason":"customer request"}`))
	refundRec := httptest.NewRecorder()
	suite.router.ServeHTTP(refundRec, refundReq)

	if refundRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", refundRec.Code, refundRec.Body.String())
	}
}

func TestGetBalance_Success(t *testing.T) {
	suite := newHandlerTestSuite()
	req := authRequest(http.MethodGet, "/api/v1/merchants/m_001/balance", nil)
	rec := httptest.NewRecorder()

	suite.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestMetrics_ReturnsJSON(t *testing.T) {
	suite := newHandlerTestSuite()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()

	suite.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("expected json content type, got %s", rec.Header().Get("Content-Type"))
	}
}
