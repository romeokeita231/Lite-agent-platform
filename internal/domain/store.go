package domain

import "context"

type Store interface {
	CreateTenant(ctx context.Context, tenant Tenant) error
	GetTenant(ctx context.Context, id string) (Tenant, error)
}
