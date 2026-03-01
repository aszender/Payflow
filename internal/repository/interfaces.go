package repository

import "context"

type PaymentRepository interface {
	Create(ctx context.Context, p interface{}) error
	GetByID(ctx context.Context, id string) (interface{}, error)
}
