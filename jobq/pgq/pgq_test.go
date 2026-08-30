package pgq

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/rustwizard/cleargo/jobq"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Shared test container (single PostgreSQL instance for the package).
var (
	sharedDSN     string
	sharedCleanup func()
	tableCounter  int64
)

func TestMain(m *testing.M) {
	// Start a shared container unless running in short mode.
	if !testing.Short() {
		dsn, cleanup, err := startSharedPostgres(context.Background())
		if err != nil {
			fmt.Fprintf(os.Stderr, "pgq: shared postgres unavailable (%v); integration tests will be skipped\n", err)
		} else {
			sharedDSN = dsn
			sharedCleanup = cleanup
		}
	}
	code := m.Run()
	if sharedCleanup != nil {
		sharedCleanup()
	}
	os.Exit(code)
}

// startSharedPostgres starts a PostgreSQL container and returns its DSN.
func startSharedPostgres(ctx context.Context) (string, func(), error) {
	container, err := postgres.Run(
		ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("pgq_test"),
		postgres.WithUsername("pgq"),
		postgres.WithPassword("pgq"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		return "", func() {}, fmt.Errorf("container start: %w", err)
	}
	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		return "", func() {}, fmt.Errorf("connection string: %w", err)
	}
	// Wait for the DB to accept connections.
	var conn *pgx.Conn
	for i := 0; i < 60; i++ {
		conn, err = pgx.Connect(ctx, connStr)
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		_ = container.Terminate(ctx)
		return "", func() {}, fmt.Errorf("postgres not ready: %w", err)
	}
	_ = conn.Close(ctx)
	cleanup := func() {
		if err := container.Terminate(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "pgq: warning: container termination failed: %v\n", err)
		}
	}
	return connStr, cleanup, nil
}

// requireShared returns the shared DSN or skips the test.
func requireShared(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if sharedDSN == "" {
		t.Skip("shared postgres unavailable (Docker not running?)")
	}
	return sharedDSN
}

// newQueue creates a Postgres queue on a fresh per‑test table.
func newQueue(t *testing.T, cfg Config) (*Postgres, *sql.DB) {
	t.Helper()
	dsn := requireShared(t)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	if cfg.Table == "" {
		n := atomic.AddInt64(&tableCounter, 1)
		cfg.Table = fmt.Sprintf("tq_t%d", n)
	}
	// Ensure a clean table.
	ctx := context.Background()
	_, err = db.ExecContext(ctx, "DROP TABLE IF EXISTS "+cfg.Table)
	require.NoError(t, err)

	// Load the embedded migration and replace the table name.
	raw, err := migrations.ReadFile("000001_init.sql")
	require.NoError(t, err)
	ddl := strings.ReplaceAll(string(raw), "jobq_jobs", cfg.Table)
	_, err = db.ExecContext(ctx, ddl)
	require.NoError(t, err)

	q, err := New(db, cfg)
	require.NoError(t, err)
	t.Logf("using table %s", cfg.Table)
	return q, db
}

// rowStatus fetches the status column for a job.
func rowStatus(t *testing.T, db *sql.DB, table string, id int64) jobq.Status {
	t.Helper()
	var s jobq.Status
	err := db.QueryRowContext(context.Background(), fmt.Sprintf("SELECT status FROM %s WHERE id=$1", table), id).Scan(&s)
	require.NoError(t, err)
	return s
}

// ---------------------------------------------------------------------------
// Unit tests (no DB required).
// ---------------------------------------------------------------------------

type noopDB struct{}

func (noopDB) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, errors.New("not implemented")
}
func (noopDB) QueryRowContext(context.Context, string, ...any) *sql.Row { return nil }

func TestConfig_WithDefaults(t *testing.T) {
	cases := []struct {
		name      string
		in        Config
		wantTable string
		wantMax   int
		wantStale time.Duration
	}{{
		name:      "zero",
		in:        Config{},
		wantTable: "jobq_jobs", wantMax: 3, wantStale: 5 * time.Minute,
	}, {
		name:      "custom table",
		in:        Config{Table: "my_jobs"},
		wantTable: "my_jobs", wantMax: 3, wantStale: 5 * time.Minute,
	}, {
		name:      "custom max",
		in:        Config{MaxAttempts: 7},
		wantTable: "jobq_jobs", wantMax: 7, wantStale: 5 * time.Minute,
	}, {
		name:      "custom stale",
		in:        Config{StaleAfter: time.Second},
		wantTable: "jobq_jobs", wantMax: 3, wantStale: time.Second,
	}, {
		name:      "neg max -> default",
		in:        Config{MaxAttempts: -1},
		wantTable: "jobq_jobs", wantMax: 3, wantStale: 5 * time.Minute,
	}, {
		name:      "neg stale -> default",
		in:        Config{StaleAfter: -time.Second},
		wantTable: "jobq_jobs", wantMax: 3, wantStale: 5 * time.Minute,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.withDefaults()
			require.Equal(t, tc.wantTable, got.Table)
			require.Equal(t, tc.wantMax, got.MaxAttempts)
			require.Equal(t, tc.wantStale, got.StaleAfter)
		})
	}
}

func TestValidateTableName(t *testing.T) {
	valid := []string{"jobq_jobs", "a", "_x", "jobs2", "a_b_c"}
	for _, name := range valid {
		require.NoError(t, validateTableName(name), "expected valid: %s", name)
	}
	// Exactly 63 characters is valid.
	require.NoError(t, validateTableName(strings.Repeat("a", 63)))
	invalid := []string{"", "Job", "job-q", "job.q", "1abc", "job job", "job\"q", strings.Repeat("a", 64)}
	for _, name := range invalid {
		require.Error(t, validateTableName(name), "expected invalid: %s", name)
	}
}

func TestNew_Validation(t *testing.T) {
	// New validates table name but does not touch DB.
	_, err := New(noopDB{}, Config{Table: "Bad Name"})
	require.Error(t, err)
	_, err = New(noopDB{}, Config{Table: "good_name"})
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Integration tests (shared PostgreSQL).
// ---------------------------------------------------------------------------

func TestMigration_Schema(t *testing.T) {
	dsn := requireShared(t)
	ctx := context.Background()
	ver, err := Migrate(ctx, dsn, 10*time.Second)
	require.NoError(t, err)
	require.GreaterOrEqual(t, ver, 1)

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer db.Close()

	var n int
	// Verify expected columns exist in the core table.
	q := `SELECT count(*) FROM information_schema.columns WHERE table_name='jobq_jobs' AND column_name IN ('id','key','payload','status','attempts','max_attempts','error','created_at','started_at','finished_at')`
	err = db.QueryRowContext(ctx, q).Scan(&n)
	require.NoError(t, err)
	require.Equal(t, 10, n)
	// Verify constraint and index.
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint WHERE conname='jobq_jobs_status_chk'`).Scan(&n)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname='jobq_jobs_pending_idx'`).Scan(&n)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func TestEnqueue_Claim_Ack(t *testing.T) {
	q, db := newQueue(t, Config{})
	ctx := context.Background()

	ok, err := q.Enqueue(ctx, "job-1", map[string]any{"foo": "bar", "n": 42})
	require.NoError(t, err)
	require.True(t, ok)

	job, err := q.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "job-1", job.Key)
	require.Equal(t, jobq.Processing, job.Status)
	require.Equal(t, 1, job.Attempts)
	require.NotNil(t, job.StartedAt)
	require.Equal(t, "bar", job.Payload["foo"])
	require.EqualValues(t, 42, job.Payload["n"])

	// Ack the job.
	require.NoError(t, q.Ack(ctx, job.ID))
	require.Equal(t, jobq.Done, rowStatus(t, db, q.table, job.ID))
	var fin sql.NullTime
	err = db.QueryRowContext(ctx, fmt.Sprintf("SELECT finished_at FROM %s WHERE id=$1", q.table), job.ID).Scan(&fin)
	require.NoError(t, err)
	require.True(t, fin.Valid)

	// Queue empty now.
	_, err = q.Claim(ctx)
	require.ErrorIs(t, err, jobq.ErrNoJobs)
}

func TestEnqueue_Idempotent(t *testing.T) {
	q, db := newQueue(t, Config{})
	ctx := context.Background()
	ok1, err := q.Enqueue(ctx, "dup", map[string]any{"v": 1})
	require.NoError(t, err)
	require.True(t, ok1)
	ok2, err := q.Enqueue(ctx, "dup", map[string]any{"v": 2})
	require.NoError(t, err)
	require.False(t, ok2)

	var cnt int
	require.NoError(t, db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s", q.table)).Scan(&cnt))
	require.Equal(t, 1, cnt)
	var payload []byte
	require.NoError(t, db.QueryRowContext(ctx, fmt.Sprintf("SELECT payload FROM %s WHERE key='dup'", q.table)).Scan(&payload))
	require.Contains(t, string(payload), `"v":1`)
}

func TestEnqueue_EmptyKey(t *testing.T) {
	q, err := New(noopDB{}, Config{})
	require.NoError(t, err)
	ok, err := q.Enqueue(context.Background(), "", nil)
	require.Error(t, err)
	require.False(t, ok)
}

func TestClaim_NoJobs(t *testing.T) {
	q, db := newQueue(t, Config{})
	ctx := context.Background()
	// Empty queue.
	_, err := q.Claim(ctx)
	require.ErrorIs(t, err, jobq.ErrNoJobs)

	// Insert a job and artificially exhaust attempts.
	ok, err := q.Enqueue(ctx, "exhausted", nil)
	require.NoError(t, err)
	require.True(t, ok)
	// Set attempts to max_attempts.
	_, err = db.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET attempts = max_attempts WHERE key='exhausted'", q.table))
	require.NoError(t, err)
	_, err = q.Claim(ctx)
	require.ErrorIs(t, err, jobq.ErrNoJobs)
}

func TestFail_RetryThenTerminal(t *testing.T) {
	q, db := newQueue(t, Config{MaxAttempts: 3})
	ctx := context.Background()
	ok, err := q.Enqueue(ctx, "flaky", nil)
	require.NoError(t, err)
	require.True(t, ok)

	// First fail → pending.
	job, err := q.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, job.Attempts)
	require.NoError(t, q.Fail(ctx, job.ID, "boom-1"))
	require.Equal(t, jobq.Pending, rowStatus(t, db, q.table, job.ID))

	// Second fail → pending.
	job, err = q.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, job.Attempts)
	require.NoError(t, q.Fail(ctx, job.ID, "boom-2"))
	require.Equal(t, jobq.Pending, rowStatus(t, db, q.table, job.ID))

	// Third fail → failed.
	job, err = q.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, job.Attempts)
	require.NoError(t, q.Fail(ctx, job.ID, "boom-3"))
	require.Equal(t, jobq.Failed, rowStatus(t, db, q.table, job.ID))

	// No further claims.
	_, err = q.Claim(ctx)
	require.ErrorIs(t, err, jobq.ErrNoJobs)
}

func TestFail_Truncation(t *testing.T) {
	q, db := newQueue(t, Config{MaxAttempts: 1})
	ctx := context.Background()
	_, _ = q.Enqueue(ctx, "long-err", nil)
	job, err := q.Claim(ctx)
	require.NoError(t, err)
	long := strings.Repeat("x", 3000)
	require.NoError(t, q.Fail(ctx, job.ID, long))
	var stored string
	err = db.QueryRowContext(ctx, fmt.Sprintf("SELECT error FROM %s WHERE id=$1", q.table), job.ID).Scan(&stored)
	require.NoError(t, err)
	require.Len(t, stored, 2000)
}

func TestAck_NotProcessing(t *testing.T) {
	q, db := newQueue(t, Config{})
	ctx := context.Background()
	// Enqueue a pending job.
	ok, err := q.Enqueue(ctx, "a1", nil)
	require.NoError(t, err)
	require.True(t, ok)
	var id int64
	err = db.QueryRowContext(ctx, fmt.Sprintf("SELECT id FROM %s WHERE key='a1'", q.table)).Scan(&id)
	require.NoError(t, err)
	err = q.Ack(ctx, id)
	require.ErrorIs(t, err, ErrNotProcessing)
	// Ack a non‑existent job.
	err = q.Ack(ctx, 99999)
	require.ErrorIs(t, err, ErrNotProcessing)
}

func TestFail_NotProcessing(t *testing.T) {
	q, db := newQueue(t, Config{})
	ctx := context.Background()
	ok, err := q.Enqueue(ctx, "f1", nil)
	require.NoError(t, err)
	require.True(t, ok)
	var id int64
	err = db.QueryRowContext(ctx, fmt.Sprintf("SELECT id FROM %s WHERE key='f1'", q.table)).Scan(&id)
	require.NoError(t, err)
	err = q.Fail(ctx, id, "oops")
	require.ErrorIs(t, err, ErrNotProcessing)
}

func TestReclaimStale(t *testing.T) {
	q, db := newQueue(t, Config{StaleAfter: 5 * time.Minute})
	ctx := context.Background()

	// Stale job.
	_, _ = q.Enqueue(ctx, "stale", nil)
	staleJob, err := q.Claim(ctx)
	require.NoError(t, err)
	// Fresh job.
	_, _ = q.Enqueue(ctx, "fresh", nil)
	freshJob, err := q.Claim(ctx)
	require.NoError(t, err)

	// Age the stale job.
	_, err = db.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET started_at = now() - interval '10 minutes' WHERE id=$1", q.table), staleJob.ID)
	require.NoError(t, err)

	reclaimed, err := q.ReclaimStale(ctx, 5*time.Minute)
	require.NoError(t, err)
	require.EqualValues(t, 1, reclaimed)
	require.Equal(t, jobq.Pending, rowStatus(t, db, q.table, staleJob.ID))
	require.Equal(t, jobq.Processing, rowStatus(t, db, q.table, freshJob.ID))

	// Using timeout=0 falls back to configured staleAfter (5m), no further reclamation.
	reclaimed, err = q.ReclaimStale(ctx, 0)
	require.NoError(t, err)
	require.EqualValues(t, 0, reclaimed)
}

func TestConcurrent_ClaimNoDouble(t *testing.T) {
	const (
		jobs    = 200
		workers = 16
	)
	q, _ := newQueue(t, Config{MaxAttempts: 5})
	ctx := context.Background()
	for i := 0; i < jobs; i++ {
		ok, err := q.Enqueue(ctx, fmt.Sprintf("c-%d", i), nil)
		require.NoError(t, err)
		require.True(t, ok)
	}
	claimed := make(chan int64, jobs)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				job, err := q.Claim(ctx)
				if err != nil {
					if errors.Is(err, jobq.ErrNoJobs) {
						return
					}
					t.Errorf("claim error: %v", err)
					return
				}
				claimed <- job.ID
			}
		}()
	}
	wg.Wait()
	close(claimed)
	seen := make(map[int64]int)
	total := 0
	for id := range claimed {
		seen[id]++
		total++
	}
	require.Equal(t, jobs, total)
	for id, c := range seen {
		require.Equal(t, 1, c, "job %d claimed %d times", id, c)
	}
}

func TestConcurrent_EnqueueSameKey(t *testing.T) {
	const workers = 32
	q, db := newQueue(t, Config{})
	ctx := context.Background()
	results := make(chan bool, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := q.Enqueue(ctx, "contested", map[string]any{"i": i})
			require.NoError(t, err)
			results <- ok
		}(i)
	}
	wg.Wait()
	close(results)
	winners := 0
	for ok := range results {
		if ok {
			winners++
		}
	}
	require.Equal(t, 1, winners)
	var cnt int
	require.NoError(t, db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s", q.table)).Scan(&cnt))
	require.Equal(t, 1, cnt)
}

func TestConcurrent_MixedOps(t *testing.T) {
	const (
		enqueues = 300
		workers  = 8
	)
	q, db := newQueue(t, Config{MaxAttempts: 3})
	ctx := context.Background()
	var wg sync.WaitGroup

	// Enqueuers.
	for i := 0; i < enqueues; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q.Enqueue(ctx, fmt.Sprintf("m-%d", i), nil)
		}(i)
	}

	// Workers.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				job, err := q.Claim(ctx)
				if err != nil {
					return
				}
				if i%3 == 0 {
					q.Fail(ctx, job.ID, fmt.Sprintf("w%d-%d", w, i))
				} else {
					q.Ack(ctx, job.ID)
				}
			}
		}(w)
	}

	wg.Wait()

	// Invariants.
	var total int
	require.NoError(t, db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s", q.table)).Scan(&total))
	require.Equal(t, enqueues, total)
	var proc int
	require.NoError(t, db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE status='processing'", q.table)).Scan(&proc))
	require.Equal(t, 0, proc)
	var done, failed, pending int
	require.NoError(t, db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE status='done'", q.table)).Scan(&done))
	require.NoError(t, db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE status='failed'", q.table)).Scan(&failed))
	require.NoError(t, db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE status='pending'", q.table)).Scan(&pending))
	require.Equal(t, enqueues, done+failed+pending)
}
