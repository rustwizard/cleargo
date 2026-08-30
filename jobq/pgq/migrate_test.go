package pgq

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func startPostgres(t *testing.T) (string, func()) {
	t.Helper()

	// These tests need Docker to run a PostgreSQL container via testcontainers-go.
	// Skip them in short mode so CI can run the remaining unit tests.
	if testing.Short() {
		t.Skip("skipping migration integration test in short mode")
	}

	ctx := context.Background()

	container, err := postgres.Run(
		ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("tortuga_test"),
		postgres.WithUsername("tortuga"),
		postgres.WithPassword("tortuga"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("failed to start postgres container: %v", err)
	}

	// wait for postgres to be ready
	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("failed to get connection string: %v", err)
	}

	// retry connection until ready
	var conn *pgx.Conn
	for i := 0; i < 30; i++ {
		conn, err = pgx.Connect(ctx, connStr)
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("postgres not ready after 15s: %v", err)
	}
	_ = conn.Close(ctx)

	dsn := connStr

	cleanup := func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("warning: failed to terminate container: %v", err)
		}
	}

	return dsn, cleanup
}

func TestMigrate(t *testing.T) {
	dsn, cleanup := startPostgres(t)
	defer cleanup()

	ctx := context.Background()

	ver, err := Migrate(ctx, dsn, 2*time.Second)
	require.NoError(t, err, "can't migrate")
	t.Log("version", ver)
}
