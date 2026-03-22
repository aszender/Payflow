package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aszender/payflow/internal/domain"
)

type BankChargeRequest struct {
	TransactionID string `json:"transaction_id"`
	MerchantID    string `json:"merchant_id"`
	AmountCents   int64  `json:"amount_cents"`
	Currency      string `json:"currency"`
}

type BankChargeResponse struct {
	Approved bool   `json:"approved"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
}

type BankClient interface {
	Charge(context.Context, BankChargeRequest) (*BankChargeResponse, error)
}

type SimulatedBankClient struct {
	Latency       time.Duration
	RejectAmounts map[int64]bool
	FailAmounts   map[int64]bool
}

func (c *SimulatedBankClient) Charge(ctx context.Context, req BankChargeRequest) (*BankChargeResponse, error) {
	latency := c.Latency
	if latency <= 0 {
		latency = 150 * time.Millisecond
	}

	select {
	case <-ctx.Done():
		return nil, domain.ErrBankTimeout
	case <-time.After(latency):
	}

	if c.FailAmounts != nil && c.FailAmounts[req.AmountCents] {
		return nil, domain.ErrBankUnavailable
	}
	if c.RejectAmounts != nil && c.RejectAmounts[req.AmountCents] {
		return &BankChargeResponse{Approved: false, Code: "DECLINED", Message: "transaction declined"}, domain.ErrBankRejected
	}

	return &BankChargeResponse{Approved: true, Code: "APPROVED"}, nil
}

type HTTPBankClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

func NewHTTPBankClient(baseURL string, timeout time.Duration) *HTTPBankClient {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	return &HTTPBankClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: timeout,
		},
	}
}

func (c *HTTPBankClient) Charge(ctx context.Context, req BankChargeRequest) (*BankChargeResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal bank request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/charge", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build bank request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, domain.ErrBankTimeout
		}
		return nil, domain.ErrBankUnavailable
	}
	defer resp.Body.Close()

	var bankResp BankChargeResponse
	if err := json.NewDecoder(resp.Body).Decode(&bankResp); err != nil {
		return nil, fmt.Errorf("decode bank response: %w", err)
	}

	switch {
	case resp.StatusCode >= 500:
		return nil, domain.ErrBankUnavailable
	case resp.StatusCode >= 400:
		if bankResp.Message == "" {
			bankResp.Message = "bank rejected the transaction"
		}
		return &bankResp, fmt.Errorf("%w: %s", domain.ErrBankRejected, bankResp.Message)
	case !bankResp.Approved:
		if bankResp.Message == "" {
			bankResp.Message = "bank rejected the transaction"
		}
		return &bankResp, fmt.Errorf("%w: %s", domain.ErrBankRejected, bankResp.Message)
	default:
		return &bankResp, nil
	}
}
