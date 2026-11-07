package repository

import (
	"context"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
)

type tenantKey struct{}

func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenantID)
}

func TenantOf(ctx context.Context) (string, error) {
	tenantID, ok := ctx.Value(tenantKey{}).(string)
	if !ok || tenantID == "" {
		return "", domain.ErrNoTenant
	}
	return tenantID, nil
}
