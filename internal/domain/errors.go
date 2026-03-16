package domain

import "errors"

// Sentinel errors for the domain layer.
// These are used across services and handlers for consistent error handling.
// Handlers map these to HTTP status codes; services return them from business logic.

var (
	// Merchant errors
	ErrMerchantNotFound = errors.New("merchant not found")
	ErrMerchantInactive = errors.New("merchant account is not active")

	// Transaction errors
	ErrTransactionNotFound  = errors.New("transaction not found")
	ErrInvalidAmount        = errors.New("amount must be positive")
	ErrInvalidCurrency      = errors.New("unsupported currency")
	ErrCannotRefund         = errors.New("can only refund completed transactions")
	ErrDuplicateTransaction = errors.New("duplicate transaction (idempotency key already used)")
	ErrTransactionExpired   = errors.New("transaction has expired")
	ErrInvalidTransition    = errors.New("invalid status transition")

	// Processing errors
	ErrBankTimeout        = errors.New("bank API timed out")
	ErrBankUnavailable    = errors.New("bank API unavailable")
	ErrBankRejected       = errors.New("bank rejected the transaction")
	ErrAmountExceedsLimit = errors.New("amount exceeds transfer limit")
	ErrInsufficientFunds  = errors.New("insufficient funds")

	// Rate limiting
	ErrRateLimited = errors.New("too many requests")

	// Auth
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
)
