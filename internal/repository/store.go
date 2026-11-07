package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
)

var _ domain.Store = (*Store)(nil)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// CreateTenant is tenant-scoped: the tenant ID must match the context tenant.
func (s *Store) CreateTenant(ctx context.Context, tenant domain.Tenant) error {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return err
	}
	if tenant.ID != tenantID {
		return domain.ErrNotFound
	}
	_, err = s.pool.Exec(ctx, `insert into tenants (id, name) values ($1, $2)`, tenant.ID, tenant.Name)
	return err
}

func (s *Store) GetTenant(ctx context.Context, id string) (domain.Tenant, error) {
	tenantID, err := TenantOf(ctx)
	if err != nil {
		return domain.Tenant{}, err
	}
	var tenant domain.Tenant
	err = s.pool.QueryRow(ctx,
		`select id, name, created_at from tenants where id = $1 and id = $2`,
		id, tenantID,
	).Scan(&tenant.ID, &tenant.Name, &tenant.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Tenant{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Tenant{}, err
	}
	return tenant, nil
}
