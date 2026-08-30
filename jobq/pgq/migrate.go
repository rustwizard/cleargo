package pgq

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/tern/v2/migrate"
)

//go:embed *.sql
var migrations embed.FS

// versionTable tracks applied pgq migrations separately from any
// application-level schema_version table. It lives in the public schema;
// if your app uses a different schema, adjust this constant.
const (
	versionTable         = "public.jobq_schema_version"
	failedMigrateVersion = -1
)

func Migrate(ctx context.Context, dsn string, timeout time.Duration) (int, error) {
	if timeout <= 0 {
		return failedMigrateVersion, errors.New("migrations: timeout must be positive")
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return failedMigrateVersion, fmt.Errorf("migrations: failed to connect to postgres: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	options := &migrate.MigratorOptions{DisableTx: false}
	migrator, err := migrate.NewMigratorEx(ctx, conn, versionTable, options)
	if err != nil {
		return failedMigrateVersion, fmt.Errorf("migrations: failed to init migrator: %w", err)
	}

	if err := migrator.LoadMigrations(migrations); err != nil {
		return failedMigrateVersion, fmt.Errorf("migrations: failed to load migrations: %w", err)
	}

	if err := migrator.Migrate(ctx); err != nil {
		return failedMigrateVersion, fmt.Errorf("migrations: failed to migrate: %w", err)
	}

	version, err := migrator.GetCurrentVersion(ctx)
	if err != nil {
		return failedMigrateVersion, fmt.Errorf("migrations: failed to get version: %w", err)
	}

	return int(version), nil
}
