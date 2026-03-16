package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aszender/payflow/internal/domain"
	"github.com/aszender/payflow/internal/metrics"
	"github.com/aszender/payflow/internal/service"
)

// ============================================================
// Response Helpers
// ============================================================

type APIResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   *APIError   `json:"error,omitempty"`
	Meta    *APIMeta    `json:"meta,omitempty"`
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type APIMeta struct {
	Total  int `json:"total,omitempty"`
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(APIResponse{Success: true, Data: data})
}

func writeJSONWithMeta(w http.ResponseWriter, status int, data interface{}, meta APIMeta) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(APIResponse{Success: true, Data: data, Meta: &meta})
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(APIResponse{
		Success: false,
		Error:   &APIError{Code: code, Message: message},
	})
}

// mapDomainError maps domain errors to HTTP status codes.
func mapDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrMerchantNotFound):
		writeError(w, http.StatusNotFound, "MERCHANT_NOT_FOUND", err.Error())
	case errors.Is(err, domain.ErrTransactionNotFound):
		writeError(w, http.StatusNotFound, "TRANSACTION_NOT_FOUND", err.Error())
	case errors.Is(err, domain.ErrMerchantInactive):
		writeError(w, http.StatusForbidden, "MERCHANT_INACTIVE", err.Error())
	case errors.Is(err, domain.ErrInvalidAmount):
		writeError(w, http.StatusBadRequest, "INVALID_AMOUNT", err.Error())
	case errors.Is(err, domain.ErrInvalidCurrency):
		writeError(w, http.StatusBadRequest, "INVALID_CURRENCY", err.Error())
	case errors.Is(err, domain.ErrAmountExceedsLimit):
		writeError(w, http.StatusBadRequest, "AMOUNT_EXCEEDS_LIMIT", err.Error())
	case errors.Is(err, domain.ErrCannotRefund):
		writeError(w, http.StatusConflict, "CANNOT_REFUND", err.Error())
	case errors.Is(err, domain.ErrInsufficientFunds):
		writeError(w, http.StatusConflict, "INSUFFICIENT_FUNDS", err.Error())
	case errors.Is(err, domain.ErrBankTimeout):
		writeError(w, http.StatusGatewayTimeout, "BANK_TIMEOUT", err.Error())
	case errors.Is(err, domain.ErrBankUnavailable), errors.Is(err, service.ErrCircuitOpen):
		writeError(w, http.StatusServiceUnavailable, "BANK_UNAVAILABLE", err.Error())
	case errors.Is(err, domain.ErrBankRejected):
		writeError(w, http.StatusPaymentRequired, "BANK_REJECTED", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an unexpected error occurred")
	}
}

// ============================================================
// Transaction Handler
// ============================================================

type TransactionHandler struct {
	svc *service.PaymentService
}

func NewTransactionHandler(svc *service.PaymentService) *TransactionHandler {
	return &TransactionHandler{svc: svc}
}

// POST /api/v1/transactions
func (h *TransactionHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MerchantID  string  `json:"merchant_id"`
		Amount      float64 `json:"amount"`
		Currency    string  `json:"currency"`
		Description string  `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "could not parse request body")
		return
	}

	// Validation
	if req.MerchantID == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "merchant_id is required")
		return
	}
	if req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "amount must be positive")
		return
	}
	if req.Currency == "" {
		req.Currency = "CAD" // default
	}

	idempotencyKey := r.Header.Get("X-Idempotency-Key")

	tx, err := h.svc.CreateTransaction(r.Context(), service.CreateTransactionInput{
		MerchantID:     req.MerchantID,
		Amount:         req.Amount,
		Currency:       req.Currency,
		IdempotencyKey: idempotencyKey,
		Description:    req.Description,
	})
	if err != nil {
		mapDomainError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, tx)
}

// GET /api/v1/transactions/{id}
func (h *TransactionHandler) GetByID(w http.ResponseWriter, r *http.Request) {
	txID := chi.URLParam(r, "id")

	tx, err := h.svc.GetTransaction(r.Context(), txID)
	if err != nil {
		mapDomainError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, tx)
}

// POST /api/v1/transactions/{id}/refund
func (h *TransactionHandler) Refund(w http.ResponseWriter, r *http.Request) {
	txID := chi.URLParam(r, "id")

	var req struct {
		Reason string `json:"reason"`
	}
	json.NewDecoder(r.Body).Decode(&req) // reason is optional

	tx, err := h.svc.RefundTransaction(r.Context(), txID, req.Reason)
	if err != nil {
		mapDomainError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, tx)
}

// GET /api/v1/transactions/{id}/events
func (h *TransactionHandler) GetEvents(w http.ResponseWriter, r *http.Request) {
	txID := chi.URLParam(r, "id")

	events, err := h.svc.GetTransactionHistory(r.Context(), txID)
	if err != nil {
		mapDomainError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, events)
}

// GET /api/v1/merchants/{id}/transactions
func (h *TransactionHandler) ListByMerchant(w http.ResponseWriter, r *http.Request) {
	merchantID := chi.URLParam(r, "id")

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	params := domain.NewListParams(limit, offset)

	txns, total, err := h.svc.ListTransactions(r.Context(), merchantID, params)
	if err != nil {
		mapDomainError(w, err)
		return
	}

	writeJSONWithMeta(w, http.StatusOK, txns, APIMeta{
		Total: total, Limit: params.Limit, Offset: params.Offset,
	})
}

// ============================================================
// Merchant Handler
// ============================================================

type MerchantHandler struct {
	svc *service.PaymentService
}

func NewMerchantHandler(svc *service.PaymentService) *MerchantHandler {
	return &MerchantHandler{svc: svc}
}

// GET /api/v1/merchants/{id}/balance
func (h *MerchantHandler) GetBalance(w http.ResponseWriter, r *http.Request) {
	merchantID := chi.URLParam(r, "id")

	merchant, err := h.svc.GetMerchantBalance(r.Context(), merchantID)
	if err != nil {
		mapDomainError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"merchant_id": merchant.ID,
		"name":        merchant.Name,
		"balance":     merchant.Balance,
		"currency":    merchant.Currency,
	})
}

// ============================================================
// Health Handler
// ============================================================

type healthChecker interface {
	HealthCheck(context.Context) error
}

type HealthHandler struct {
	db      healthChecker
	version string
	startAt time.Time
}

func NewHealthHandler(db healthChecker, version string) *HealthHandler {
	return &HealthHandler{db: db, version: version, startAt: time.Now()}
}

func (h *HealthHandler) Check(w http.ResponseWriter, r *http.Request) {
	dbStatus := "healthy"
	httpStatus := http.StatusOK

	if h.db != nil {
		if err := h.db.HealthCheck(r.Context()); err != nil {
			dbStatus = fmt.Sprintf("unhealthy: %v", err)
			httpStatus = http.StatusServiceUnavailable
		}
	}

	status := "healthy"
	if httpStatus != http.StatusOK {
		status = "degraded"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  status,
		"version": h.version,
		"uptime":  time.Since(h.startAt).Truncate(time.Second).String(),
		"checks": map[string]string{
			"database": dbStatus,
		},
	})
}

// ============================================================
// Metrics Handler
// ============================================================

type metricsSnapshotter interface {
	GetSnapshot() metrics.Snapshot
}

type MetricsHandler struct {
	m metricsSnapshotter
}

func NewMetricsHandler(m metricsSnapshotter) *MetricsHandler {
	return &MetricsHandler{m: m}
}

func (h *MetricsHandler) Get(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if h.m == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{})
		return
	}
	json.NewEncoder(w).Encode(h.m.GetSnapshot())
}

// ============================================================
// Router Setup
// ============================================================

func SetupRoutes(
	r *chi.Mux,
	txHandler *TransactionHandler,
	merchantHandler *MerchantHandler,
	healthHandler *HealthHandler,
	metricsHandler *MetricsHandler,
	merchantRepo interface {
		GetByAPIKey(ctx interface{}, key string) (interface{}, error)
	},
) {
	// API v1 — authenticated
	r.Route("/api/v1", func(r chi.Router) {
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
}
