package repository

import (
	"context"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
)

func WithTenant(ctx context.Context, tenantID string) context.Context {
	return domain.WithTenant(ctx, tenantID)
}

func TenantOf(ctx context.Context) (string, error) {
	return domain.TenantOf(ctx)
}
