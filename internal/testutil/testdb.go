package testutil

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romeokeita231/Lite-agent-platform/internal/migrate"
)

type Env struct {
	Pool *pgxpool.Pool
	URL  string
}

func URL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	return url
}

func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), URL(t))
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

const testLockKey int64 = 0x74657374

func New(t *testing.T) *Env {
	t.Helper()
	url := URL(t)
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lock connection: %v", err)
	}
	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1)`, testLockKey); err != nil {
		t.Fatalf("acquire test lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `select pg_advisory_unlock($1)`, testLockKey)
		conn.Release()
	})
	if err := migrate.Run(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	Truncate(t, pool)
	return &Env{Pool: pool, URL: url}
}

func Truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `select tablename from pg_tables where schemaname = 'public' and tablename <> 'schema_migrations'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect tables: %v", err)
	}
	for _, table := range tables {
		stmt := "truncate table " + pgx.Identifier{table}.Sanitize() + " restart identity cascade"
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
}
