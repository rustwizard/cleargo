package pgq

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
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

// Time bounds for the shared container startup and readiness check.
const (
	postgresWaitTimeout   = 30 * time.Second
	postgresRetryInterval = 500 * time.Millisecond
)

func TestMain(m *testing.M) {
	// Parse flags first; testing.Short() panics otherwise.
	flag.Parse()

	// Allow running integration tests against an external Postgres
	// (e.g. in CI) without Docker: PGQ_TEST_DSN=postgres://user:pass@host:5432/db
	if dsn := os.Getenv("PGQ_TEST_DSN"); dsn != "" {
		sharedDSN = dsn
		fmt.Fprintf(os.Stderr, "pgq: using external postgres from PGQ_TEST_DSN\n")
	} else if !testing.Short() {
		// Start a shared container unless running in short mode.
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
		return "", func() {}, fmt.Errorf("start postgres container: %w", err)
	}

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			// Use Background here so cleanup does not depend on a canceled test context.
			if err := container.Terminate(context.Background()); err != nil {
				fmt.Fprintf(os.Stderr, "pgq: warning: terminate postgres container: %v\n", err)
			}
		})
	}

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("build connection string: %w", err)
	}

	if err := waitForPostgres(ctx, connStr); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("postgres not ready: %w", err)
	}

	return connStr, cleanup, nil
}

func waitForPostgres(ctx context.Context, connStr string) error {
	// Respect the caller's context, but also bound the wait to a fixed timeout.
	ctx, cancel := context.WithTimeout(ctx, postgresWaitTimeout)
	defer cancel()

	var lastErr error
	ticker := time.NewTicker(postgresRetryInterval)
	defer ticker.Stop()

	for {
		conn, err := pgx.Connect(ctx, connStr)
		if err == nil {
			_ = conn.Close(ctx)
			return nil
		}

		lastErr = err

		select {
		case <-ctx.Done():
			// Return both the context error and the last connection error if useful.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) && !errors.Is(lastErr, context.DeadlineExceeded) {
				return errors.Join(lastErr, ctx.Err())
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
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

	// Bound the connection pool: Postgres default max_connections is 100,
	// and an unbounded pool would blow past it under concurrent tests
	// ("sorry, too many clients already").
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(16)

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
func (noopDB) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
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

	// Original payload must be preserved (jsonb normalizes formatting,
	// so compare semantically, not textually).
	var payload []byte
	require.NoError(t, db.QueryRowContext(ctx, fmt.Sprintf("SELECT payload FROM %s WHERE key='dup'", q.table)).Scan(&payload))
	var got map[string]any
	require.NoError(t, json.Unmarshal(payload, &got))
	require.Equal(t, map[string]any{"v": float64(1)}, got)
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

	// ASCII payload: truncated to exactly 2000 chars.
	_, _ = q.Enqueue(ctx, "long-err", nil)
	job, err := q.Claim(ctx)
	require.NoError(t, err)
	require.NoError(t, q.Fail(ctx, job.ID, strings.Repeat("x", 3000)))
	var stored string
	err = db.QueryRowContext(ctx, fmt.Sprintf("SELECT error FROM %s WHERE id=$1", q.table), job.ID).Scan(&stored)
	require.NoError(t, err)
	require.Len(t, stored, 2000)

	// Cyrillic payload: truncation must not cut a multibyte rune in half.
	_, _ = q.Enqueue(ctx, "long-err-cyr", nil)
	job, err = q.Claim(ctx)
	require.NoError(t, err)
	cyr := strings.Repeat("й", 3000) // 2 bytes per rune
	require.NoError(t, q.Fail(ctx, job.ID, cyr))
	err = db.QueryRowContext(ctx, fmt.Sprintf("SELECT error FROM %s WHERE id=$1", q.table), job.ID).Scan(&stored)
	require.NoError(t, err)
	require.Equal(t, 2000, utf8.RuneCountInString(stored), "exactly 2000 runes, no broken UTF-8")
	require.True(t, utf8.ValidString(stored), "stored error must be valid UTF-8")
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
	errCh := make(chan error, workers)
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
					errCh <- err
					return
				}
				claimed <- job.ID
			}
		}()
	}
	wg.Wait()
	close(claimed)
	close(errCh)
	for err := range errCh {
		t.Errorf("claim error: %v", err)
	}
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

	errCh := make(chan error, enqueues+workers*100)
	var wg sync.WaitGroup

	// Enqueuers.
	for i := 0; i < enqueues; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := q.Enqueue(ctx, fmt.Sprintf("m-%d", i), nil); err != nil {
				errCh <- fmt.Errorf("enqueue m-%d: %w", i, err)
			}
		}(i)
	}

	// Workers: claim -> randomly ack or fail.
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
					if err := q.Fail(ctx, job.ID, fmt.Sprintf("w%d-%d", w, i)); err != nil {
						errCh <- fmt.Errorf("fail job %d: %w", job.ID, err)
					}
				} else {
					if err := q.Ack(ctx, job.ID); err != nil {
						errCh <- fmt.Errorf("ack job %d: %w", job.ID, err)
					}
				}
			}
		}(w)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("goroutine error: %v", err)
	}

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

func TestStats(t *testing.T) {
	q, _ := newQueue(t, Config{MaxAttempts: 2})
	ctx := context.Background()

	// Empty queue.
	st, err := q.Stats(ctx)
	require.NoError(t, err)
	require.Equal(t, jobq.Stats{}, st)
	depth, err := q.Depth(ctx)
	require.NoError(t, err)
	require.Zero(t, depth)

	// Three pending.
	for _, k := range []string{"s1", "s2", "s3"} {
		ok, err := q.Enqueue(ctx, k, nil)
		require.NoError(t, err)
		require.True(t, ok)
	}
	st, err = q.Stats(ctx)
	require.NoError(t, err)
	require.Equal(t, jobq.Stats{Pending: 3}, st)
	depth, err = q.Depth(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, depth)

	// Claim one -> processing, depth shrinks.
	job, err := q.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "s1", job.Key)
	st, err = q.Stats(ctx)
	require.NoError(t, err)
	require.Equal(t, jobq.Stats{Pending: 2, Processing: 1}, st)
	depth, err = q.Depth(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, depth)

	// Ack it -> done.
	require.NoError(t, q.Ack(ctx, job.ID))
	st, err = q.Stats(ctx)
	require.NoError(t, err)
	require.Equal(t, jobq.Stats{Pending: 2, Done: 1}, st)

	// Exhaust attempts of s2 (maxAttempts=2): claim, fail, claim, fail.
	for i := 0; i < 2; i++ {
		job, err = q.Claim(ctx)
		require.NoError(t, err)
		require.Equal(t, "s2", job.Key)
		require.NoError(t, q.Fail(ctx, job.ID, "boom"))
	}
	st, err = q.Stats(ctx)
	require.NoError(t, err)
	require.Equal(t, jobq.Stats{Pending: 1, Done: 1, Failed: 1}, st)
	depth, err = q.Depth(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, depth)
}

// gaugeValue returns the value of a gauge metric matching name+labels
// from gathered metric families, or -1 when absent.
func gaugeValue(mfs []*dto.MetricFamily, name, table, status string) float64 {
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			var gotTable, gotStatus string
			for _, lp := range m.GetLabel() {
				switch lp.GetName() {
				case "table":
					gotTable = lp.GetValue()
				case "status":
					gotStatus = lp.GetValue()
				}
			}
			if gotTable == table && gotStatus == status {
				return m.GetGauge().GetValue()
			}
		}
	}
	return -1
}

func TestMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	q, _ := newQueue(t, Config{Metrics: reg})
	ctx := context.Background()

	_, _ = q.Enqueue(ctx, "a", nil)
	_, _ = q.Enqueue(ctx, "b", nil)
	job, err := q.Claim(ctx) // a -> processing
	require.NoError(t, err)
	require.NoError(t, q.Ack(ctx, job.ID))
	job, err = q.Claim(ctx) // b -> processing
	require.NoError(t, err)
	require.NoError(t, q.Ack(ctx, job.ID))
	_, err = q.Claim(ctx) // drained
	require.ErrorIs(t, err, jobq.ErrNoJobs)

	// Operation counters (updated synchronously).
	require.Equal(t, 2.0, testutil.ToFloat64(q.metrics.opsTotal.WithLabelValues(q.table, "enqueue")))
	require.Equal(t, 2.0, testutil.ToFloat64(q.metrics.opsTotal.WithLabelValues(q.table, "claim")))
	require.Equal(t, 2.0, testutil.ToFloat64(q.metrics.opsTotal.WithLabelValues(q.table, "ack")))

	// Depth gauges are refreshed on scrape: pending=0, done=2.
	mfs, err := reg.Gather()
	require.NoError(t, err)
	require.Equal(t, 0.0, gaugeValue(mfs, "jobq_jobs_by_status", q.table, "pending"))
	require.Equal(t, 2.0, gaugeValue(mfs, "jobq_jobs_by_status", q.table, "done"))
	require.Equal(t, 0.0, gaugeValue(mfs, "jobq_jobs_by_status", q.table, "processing"))
	require.Equal(t, 0.0, gaugeValue(mfs, "jobq_jobs_by_status", q.table, "failed"))

	// A second scrape reflects a new state (one pending again).
	_, _ = q.Enqueue(ctx, "c", nil)
	mfs, err = reg.Gather()
	require.NoError(t, err)
	require.Equal(t, 1.0, gaugeValue(mfs, "jobq_jobs_by_status", q.table, "pending"))
}
