package migrate_test

import (
	"context"
	"os"
	"path"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romeokeita231/Lite-agent-platform/internal/migrate"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
)

func TestRunAppliesEachMigrationOnceInOrder(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()

	entries, err := os.ReadDir("sql")
	if err != nil {
		t.Fatalf("read sql dir: %v", err)
	}
	expected := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && path.Ext(entry.Name()) == ".sql" {
			expected = append(expected, entry.Name())
		}
	}
	slices.Sort(expected)
	if !slices.IsSorted(expected) {
		t.Fatalf("migrations are not ordered by filename: %v", expected)
	}

	if err := migrate.Run(ctx, pool); err != nil {
		t.Fatalf("first run: %v", err)
	}
	versions := appliedVersions(t, pool)
	if !slices.Equal(versions, expected) {
		t.Fatalf("applied versions = %v, want %v", versions, expected)
	}
	before := appliedTimes(t, pool)

	if err := migrate.Run(ctx, pool); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if after := appliedVersions(t, pool); !slices.Equal(after, expected) {
		t.Fatalf("second run changed versions: %v", after)
	}
	if after := appliedTimes(t, pool); !slices.Equal(after, before) {
		t.Fatalf("second run re-applied migrations: applied_at changed")
	}

	var tenants int
	if err := pool.QueryRow(ctx, `select count(*) from information_schema.tables where table_schema = 'public' and table_name = 'tenants'`).Scan(&tenants); err != nil {
		t.Fatalf("check tenants table: %v", err)
	}
	if tenants != 1 {
		t.Fatalf("tenants table missing")
	}
}

func appliedVersions(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select version from schema_migrations order by version`)
	if err != nil {
		t.Fatalf("query versions: %v", err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect versions: %v", err)
	}
	return versions
}

func appliedTimes(t *testing.T, pool *pgxpool.Pool) []time.Time {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select applied_at from schema_migrations order by version`)
	if err != nil {
		t.Fatalf("query applied_at: %v", err)
	}
	times, err := pgx.CollectRows(rows, pgx.RowTo[time.Time])
	if err != nil {
		t.Fatalf("collect applied_at: %v", err)
	}
	return times
}
