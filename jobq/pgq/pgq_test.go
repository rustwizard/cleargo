package pgq

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQueueMigration(t *testing.T) {
	dsn, cleanup := startPostgres(t)
	defer cleanup()

	ctx := context.Background()

	ver, err := Migrate(ctx, dsn, 2*time.Second)
	require.NoError(t, err, "can't migrate")
	t.Log("version", ver)
}
