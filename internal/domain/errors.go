package domain

import "errors"

var (
	ErrNotFound  = errors.New("not found")
	ErrRetryable = errors.New("retryable error")
)
