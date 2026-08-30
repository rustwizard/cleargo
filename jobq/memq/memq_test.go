package memq

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rustwizard/cleargo/jobq"
	"github.com/stretchr/testify/require"
)

// TestConcurrent_EnqueueSameKey verifies that when N goroutines race to
// enqueue the same key, exactly one wins and the rest get false.
// Run with: go test -race ./mem/
func TestConcurrent_EnqueueSameKey(t *testing.T) {
	const workers = 50

	m := New(3)
	ctx := context.Background()

	var wg sync.WaitGroup
	successes := make(chan bool, workers)
	errs := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := m.Enqueue(ctx, "contested-key", map[string]any{"i": i})
			if err != nil {
				errs <- err
				return
			}
			successes <- ok
		}()
	}

	wg.Wait()
	close(successes)
	close(errs)
	for err := range errs {
		t.Errorf("enqueue error: %v", err)
	}

	var count int
	for ok := range successes {
		if ok {
			count++
		}
	}

	require.Equal(t, 1, count, "exactly one goroutine must win the insert")
	require.Equal(t, 1, m.Len())
}

// TestConcurrent_ClaimNoDouble ensures that N workers claiming concurrently
// never receive the same job twice. Each job is claimed by at most one
// goroutine.
func TestConcurrent_ClaimNoDouble(t *testing.T) {
	const (
		jobs    = 100
		workers = 10
	)

	m := New(3)
	ctx := context.Background()

	// Seed the queue.
	for i := 0; i < jobs; i++ {
		m.Enqueue(ctx, fmt.Sprintf("job-%d", i), nil)
	}

	// Each worker claims in a loop until the queue is empty.
	claimed := make(chan int64, jobs)
	errs := make(chan error, workers)
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				job, err := m.Claim(ctx)
				if err != nil {
					if err == jobq.ErrNoJobs {
						return
					}
					errs <- err
					return
				}
				claimed <- job.ID
			}
		}()
	}

	wg.Wait()
	close(claimed)
	close(errs)
	for err := range errs {
		t.Errorf("claim error: %v", err)
	}

	// Collect and verify.
	seen := make(map[int64]int)
	total := 0
	for id := range claimed {
		seen[id]++
		total++
	}

	require.Equal(t, jobs, total, "all jobs must be claimed exactly once")
	for id, count := range seen {
		require.Equal(t, 1, count, "job %d claimed %d times (expected 1)", id, count)
	}
}

// TestConcurrent_MixedOps hammers Enqueue, Claim, Ack, Fail, and
// ReclaimStale simultaneously. The invariant we check:
//   - no data race (enforced by -race)
//   - Len() == total_enqueued (no jobs lost or duplicated)
//   - every job ends in exactly one terminal state
func TestConcurrent_MixedOps(t *testing.T) {
	const (
		enqueues = 200
		workers  = 8
	)

	m := New(2)
	ctx := context.Background()

	var wg sync.WaitGroup

	// Enqueuers.
	for i := 0; i < enqueues; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			m.Enqueue(ctx, fmt.Sprintf("job-%d", id), map[string]any{"id": id})
		}(i)
	}

	// Workers: claim → randomly ack or fail.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				job, err := m.Claim(ctx)
				if err != nil {
					return
				}
				if i%3 == 0 {
					m.Fail(ctx, job.ID, fmt.Sprintf("worker-%d-fail-%d", seed, i))
				} else {
					m.Ack(ctx, job.ID)
				}
			}
		}(w)
	}

	// Stale reclaimer running in parallel.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			// timeout=0 uses the queue's staleAfter; jobs are fresh, so nothing is reclaimed.
			m.ReclaimStale(ctx, 0)
		}
	}()

	wg.Wait()

	// Invariants.
	require.Equal(t, enqueues, m.Len(), "no jobs lost")

	done := m.CountByStatus(jobq.Done)
	failed := m.CountByStatus(jobq.Failed)
	pending := m.CountByStatus(jobq.Pending)
	processing := m.CountByStatus(jobq.Processing)

	// processing may be non-zero if a worker claimed but hasn't finished
	// its Ack/Fail yet... but we joined all workers, so it should be 0.
	require.Equal(t, 0, processing, "all workers joined, no stuck jobs")

	// Terminal + pending + processing = total.
	require.Equal(t, enqueues, done+failed+pending+processing)
}

// TestConcurrent_EnqueueAndClaim verifies no deadlock when enqueuers and
// claimers run simultaneously.
func TestConcurrent_EnqueueAndClaim(t *testing.T) {
	const (
		total   = 500
		workers = 5
	)

	m := New(3)
	ctx := context.Background()

	// Pre-populate half.
	for i := 0; i < total/2; i++ {
		m.Enqueue(ctx, fmt.Sprintf("pre-%d", i), nil)
	}

	var wg sync.WaitGroup

	// Enqueuers for the second half.
	for i := 0; i < total/2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			m.Enqueue(ctx, fmt.Sprintf("late-%d", id), nil)
		}(i)
	}

	// Claimers drain until empty.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				_, err := m.Claim(ctx)
				if err != nil {
					return
				}
			}
		}()
	}

	wg.Wait()

	// All jobs should be either done/failed/pending (no processing since
	// workers returned). Total count must be preserved.
	require.Equal(t, total, m.Len())
}

func TestStats(t *testing.T) {
	m := New(2)
	ctx := context.Background()

	// Empty queue.
	st, err := m.Stats(ctx)
	require.NoError(t, err)
	require.Equal(t, jobq.Stats{}, st)
	depth, err := m.Depth(ctx)
	require.NoError(t, err)
	require.Zero(t, depth)

	for _, k := range []string{"s1", "s2", "s3"} {
		ok, err := m.Enqueue(ctx, k, nil)
		require.NoError(t, err)
		require.True(t, ok)
	}
	st, err = m.Stats(ctx)
	require.NoError(t, err)
	require.Equal(t, jobq.Stats{Pending: 3}, st)
	depth, err = m.Depth(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, depth)

	job, err := m.Claim(ctx)
	require.NoError(t, err)
	st, err = m.Stats(ctx)
	require.NoError(t, err)
	require.Equal(t, jobq.Stats{Pending: 2, Processing: 1}, st)
	depth, err = m.Depth(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, depth)

	require.NoError(t, m.Ack(ctx, job.ID))
	st, err = m.Stats(ctx)
	require.NoError(t, err)
	require.Equal(t, jobq.Stats{Pending: 2, Done: 1}, st)

	// Exhaust attempts of s2 (maxAttempts=2).
	for i := 0; i < 2; i++ {
		job, err = m.Claim(ctx)
		require.NoError(t, err)
		require.NoError(t, m.Fail(ctx, job.ID, "boom"))
	}
	st, err = m.Stats(ctx)
	require.NoError(t, err)
	require.Equal(t, jobq.Stats{Pending: 1, Done: 1, Failed: 1}, st)
	depth, err = m.Depth(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, depth)
}

func TestBackoff(t *testing.T) {
	m := New(3)
	m.SetRetryBase(50 * time.Millisecond)
	ctx := context.Background()

	ok, err := m.Enqueue(ctx, "bk", nil)
	require.NoError(t, err)
	require.True(t, ok)

	job, err := m.Claim(ctx)
	require.NoError(t, err)
	require.NoError(t, m.Fail(ctx, job.ID, "e1"))

	// Deferred: not claimable and not counted in depth.
	depth, err := m.Depth(ctx)
	require.NoError(t, err)
	require.Zero(t, depth)
	_, err = m.Claim(ctx)
	require.ErrorIs(t, err, jobq.ErrNoJobs)

	// After the backoff elapses the job is claimable again (attempt 2).
	time.Sleep(120 * time.Millisecond)
	job, err = m.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, job.Attempts)

	// Without a retry base the backoff is disabled (immediate retry).
	m2 := New(3)
	_, _ = m2.Enqueue(ctx, "bk2", nil)
	j2, err := m2.Claim(ctx)
	require.NoError(t, err)
	require.NoError(t, m2.Fail(ctx, j2.ID, "e2"))
	require.NoError(t, err)
	j2, err = m2.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, j2.Attempts)
}

func TestHeartbeat(t *testing.T) {
	m := New(3, 50*time.Millisecond) // staleAfter
	m.SetLease(200 * time.Millisecond)
	ctx := context.Background()

	ok, err := m.Enqueue(ctx, "long", nil)
	require.NoError(t, err)
	require.True(t, ok)

	job, err := m.Claim(ctx)
	require.NoError(t, err)

	// Lease active past staleAfter: not reclaimed.
	time.Sleep(100 * time.Millisecond)
	n, err := m.ReclaimStale(ctx, 0)
	require.NoError(t, err)
	require.EqualValues(t, 0, n)

	// Heartbeat renews; still not reclaimed.
	require.NoError(t, m.Heartbeat(ctx, job.ID))
	time.Sleep(100 * time.Millisecond)
	n, err = m.ReclaimStale(ctx, 0)
	require.NoError(t, err)
	require.EqualValues(t, 0, n)

	// Lease expires without heartbeat: reclaimed back to pending.
	time.Sleep(200 * time.Millisecond)
	n, err = m.ReclaimStale(ctx, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	require.Equal(t, jobq.Pending, m.Get(job.ID).Status)

	// Heartbeat on a non-processing job fails.
	err = m.Heartbeat(ctx, job.ID)
	require.ErrorIs(t, err, ErrNotProcessing)
}

// TestBackoff_Deterministic drives the backoff with an injected clock instead
// of sleeping: advancing the clock past the delay makes the job claimable.
func TestBackoff_Deterministic(t *testing.T) {
	m := New(3)
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	m.SetClock(func() time.Time { return now })
	m.SetRetryBase(100 * time.Millisecond)
	ctx := context.Background()

	_, _ = m.Enqueue(ctx, "bk", nil)
	job, err := m.Claim(ctx)
	require.NoError(t, err)
	require.NoError(t, m.Fail(ctx, job.ID, "e1"))

	// Deferred until now+100ms.
	depth, _ := m.Depth(ctx)
	require.Zero(t, depth)
	_, err = m.Claim(ctx)
	require.ErrorIs(t, err, jobq.ErrNoJobs)

	// Advance halfway: still deferred.
	now = now.Add(50 * time.Millisecond)
	depth, _ = m.Depth(ctx)
	require.Zero(t, depth)

	// Advance past the delay: claimable again, attempt 2.
	now = now.Add(100 * time.Millisecond)
	job, err = m.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, job.Attempts)
}

func TestRetryCap(t *testing.T) {
	m := New(3)
	m.SetRetryBase(100 * time.Millisecond)
	m.SetRetryCap(250 * time.Millisecond)

	require.Equal(t, 100*time.Millisecond, m.backoffDelay(1))
	require.Equal(t, 200*time.Millisecond, m.backoffDelay(2))
	require.Equal(t, 250*time.Millisecond, m.backoffDelay(3)) // 400ms capped
	require.Equal(t, 250*time.Millisecond, m.backoffDelay(10))
}

func TestMaxAttempts(t *testing.T) {
	require.Equal(t, 3, New(0).MaxAttempts())
	require.Equal(t, 7, New(7).MaxAttempts())
}

func TestGetByKey(t *testing.T) {
	m := New(3)
	_, err := m.Enqueue(context.Background(), "k1", map[string]any{"a": 1})
	require.NoError(t, err)

	j := m.GetByKey("k1")
	require.NotNil(t, j)
	require.Equal(t, "k1", j.Key)
	require.Equal(t, map[string]any{"a": 1}, j.Payload)
	require.Nil(t, m.GetByKey("missing"))
}
