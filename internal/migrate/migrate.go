package migrate

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed sql/*.sql
var sqlFS embed.FS

const (
	schemaMigrationsTable = `create table if not exists schema_migrations (
	version text primary key,
	applied_at timestamptz not null default now()
)`
	migrationLockKey int64 = 0x6c697465
)

func Run(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `select pg_advisory_unlock($1)`, migrationLockKey)
	}()

	return runLocked(ctx, conn)
}

func runLocked(ctx context.Context, conn *pgxpool.Conn) error {
	if _, err := conn.Exec(ctx, schemaMigrationsTable); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	files, err := migrationFiles()
	if err != nil {
		return err
	}
	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}
	for _, file := range files {
		version := path.Base(file)
		if _, ok := applied[version]; ok {
			continue
		}
		if err := apply(ctx, conn, file, version); err != nil {
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
	}
	return nil
}

func migrationFiles() ([]string, error) {
	files, err := fs.Glob(sqlFS, "sql/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func appliedVersions(ctx context.Context, conn *pgxpool.Conn) (map[string]struct{}, error) {
	rows, err := conn.Query(ctx, `select version from schema_migrations`)
	if err != nil {
		return nil, err
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	applied := make(map[string]struct{}, len(versions))
	for _, version := range versions {
		applied[version] = struct{}{}
	}
	return applied, nil
}

func apply(ctx context.Context, conn *pgxpool.Conn, file, version string) error {
	body, err := sqlFS.ReadFile(file)
	if err != nil {
		return err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, string(body), pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `insert into schema_migrations (version) values ($1)`, version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
