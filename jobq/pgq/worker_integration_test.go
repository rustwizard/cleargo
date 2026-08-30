package pgq

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rustwizard/cleargo/jobq"
	"github.com/rustwizard/cleargo/jobq/worker"
	"github.com/stretchr/testify/require"
)

var errBoom = errors.New("boom")

// mustID returns the id of the job with the given key.
func mustID(t *testing.T, db *sql.DB, table, key string) int64 {
	t.Helper()
	var id int64
	err := db.QueryRowContext(context.Background(),
		"SELECT id FROM "+table+" WHERE key=$1", key).Scan(&id)
	require.NoError(t, err)
	return id
}

// waitStatus polls until the job reaches want or the timeout elapses.
func waitStatus(t *testing.T, db *sql.DB, table string, id int64, want jobq.Status, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rowStatus(t, db, table, id) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %d did not reach %s within %v", id, want, timeout)
}

// runWorker starts w.Run in a goroutine and returns a stop func and a channel
// that receives Run's error.
func runWorker(t *testing.T, w *worker.Worker) (stop func(), done <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- w.Run(ctx) }()
	return cancel, ch
}

func TestWorker_ProcessesJobs(t *testing.T) {
	q, _ := newQueue(t, Config{})
	ctx := context.Background()
	const n = 10
	for i := 0; i < n; i++ {
		ok, err := q.Enqueue(ctx, string(rune('a'+i)), nil)
		require.NoError(t, err)
		require.True(t, ok)
	}

	w := worker.New(q, worker.Config{Workers: 3, PollInterval: 5 * time.Millisecond},
		func(context.Context, *jobq.Job) error { return nil })

	stop, done := runWorker(t, w)
	defer func() {
		stop()
		<-done
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := q.Stats(ctx)
		require.NoError(t, err)
		if st.Done == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d jobs done", st.Done, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWorker_FailsOnError(t *testing.T) {
	q, _ := newQueue(t, Config{MaxAttempts: 1})
	ctx := context.Background()
	_, _ = q.Enqueue(ctx, "bad", nil)

	w := worker.New(q, worker.Config{Workers: 1, PollInterval: 5 * time.Millisecond},
		func(context.Context, *jobq.Job) error { return errBoom })

	stop, done := runWorker(t, w)
	defer func() {
		stop()
		<-done
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := q.Stats(ctx)
		require.NoError(t, err)
		if st.Failed == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job not failed: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWorker_ReclaimsOrphanedJob verifies that a job whose lease expires while
// a worker handles it is reclaimed and processed again (attempt 2 succeeds).
func TestWorker_ReclaimsOrphanedJob(t *testing.T) {
	q, db := newQueue(t, Config{StaleAfter: 150 * time.Millisecond, Lease: 150 * time.Millisecond})
	ctx := context.Background()
	_, _ = q.Enqueue(ctx, "orphan", nil)

	started := make(chan struct{})
	finish := make(chan struct{})
	var mu sync.Mutex
	attempts := 0

	w := worker.New(q, worker.Config{Workers: 1, PollInterval: 5 * time.Millisecond},
		func(_ context.Context, job *jobq.Job) error {
			mu.Lock()
			attempts++
			n := attempts
			mu.Unlock()
			if n == 1 {
				close(started)
				<-finish // simulate a hung/crashed worker
			}
			return nil
		})

	stop, done := runWorker(t, w)
	defer func() {
		stop()
		<-done
	}()

	<-started // first attempt is in flight

	// Let the lease expire, then reclaim manually.
	time.Sleep(300 * time.Millisecond)
	n, err := q.ReclaimStale(ctx, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	require.Equal(t, jobq.Pending, rowStatus(t, db, q.table, mustID(t, db, q.table, "orphan")))

	// Release the hung handler; the worker picks the job up for attempt 2.
	close(finish)
	id := mustID(t, db, q.table, "orphan")
	waitStatus(t, db, q.table, id, jobq.Done, 10*time.Second)

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, attempts, 2, "job must be processed at least twice")
}

// TestWorker_HeartbeatKeepsLongJob verifies that automatic heartbeats keep a
// long-running job from being reclaimed while the worker is alive.
func TestWorker_HeartbeatKeepsLongJob(t *testing.T) {
	q, db := newQueue(t, Config{StaleAfter: 100 * time.Millisecond, Lease: 200 * time.Millisecond})
	ctx := context.Background()
	_, _ = q.Enqueue(ctx, "long", nil)

	finished := make(chan struct{})
	w := worker.New(q, worker.Config{
		Workers:           1,
		PollInterval:      5 * time.Millisecond,
		ReclaimInterval:   30 * time.Millisecond,
		HeartbeatInterval: 40 * time.Millisecond,
	}, func(context.Context, *jobq.Job) error {
		time.Sleep(400 * time.Millisecond) // outlives lease+staleAfter
		close(finished)
		return nil
	})

	stop, done := runWorker(t, w)

	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("long job never finished")
	}
	stop()
	require.NoError(t, <-done)
	id := mustID(t, db, q.table, "long")
	require.Equal(t, jobq.Done, rowStatus(t, db, q.table, id))
}

// TestWorker_GracefulShutdown verifies that a handler which outlives
// ShutdownTimeout is abandoned: the job stays "processing" for a later
// ReclaimStale instead of being silently acked or failed.
func TestWorker_GracefulShutdown(t *testing.T) {
	q, db := newQueue(t, Config{StaleAfter: 30 * time.Millisecond, Lease: 30 * time.Millisecond})
	ctx := context.Background()
	_, _ = q.Enqueue(ctx, "slow", nil)

	started := make(chan struct{})
	w := worker.New(q, worker.Config{Workers: 1, PollInterval: 5 * time.Millisecond, ShutdownTimeout: 50 * time.Millisecond},
		func(jctx context.Context, _ *jobq.Job) error {
			close(started)
			<-jctx.Done()
			time.Sleep(200 * time.Millisecond) // outlives ShutdownTimeout
			return nil
		})

	stop, done := runWorker(t, w)
	<-started
	start := time.Now()
	stop()
	require.NoError(t, <-done)
	require.Less(t, time.Since(start), time.Second, "Run must not wait for the abandoned handler")

	id := mustID(t, db, q.table, "slow")
	require.Equal(t, jobq.Processing, rowStatus(t, db, q.table, id))

	// A later reclaim makes it pending again (once the short lease lapses).
	time.Sleep(100 * time.Millisecond)
	n, err := q.ReclaimStale(ctx, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	require.Equal(t, jobq.Pending, rowStatus(t, db, q.table, id))
}
