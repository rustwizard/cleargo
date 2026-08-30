package pgq

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMigrate(t *testing.T) {
	dsn := requireShared(t)

	ctx := context.Background()

	// Migrating a fresh shared database must succeed and produce >= 1 version.
	ver, err := Migrate(ctx, dsn, 30*time.Second)
	require.NoError(t, err, "can't migrate")
	require.GreaterOrEqual(t, ver, 1)

	// Migrate must be idempotent: a second run keeps the same version.
	ver2, err := Migrate(ctx, dsn, 30*time.Second)
	require.NoError(t, err, "second migrate must not fail")
	require.Equal(t, ver, ver2, "version must not change on re-migrate")

	t.Log("version", ver)
}
