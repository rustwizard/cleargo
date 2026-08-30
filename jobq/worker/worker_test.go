package worker_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rustwizard/cleargo/jobq"
	"github.com/rustwizard/cleargo/jobq/memq"
	"github.com/rustwizard/cleargo/jobq/worker"
	"github.com/stretchr/testify/require"
)

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within", timeout)
}

func TestWorker_ProcessesJobs(t *testing.T) {
	const n = 20
	m := memq.New(3)
	for i := 0; i < n; i++ {
		_, _ = m.Enqueue(context.Background(), fmt.Sprintf("j-%d", i), nil)
	}

	var processed atomic.Int64
	w := worker.New(m, worker.Config{Workers: 4, PollInterval: 5 * time.Millisecond},
		func(_ context.Context, _ *jobq.Job) error {
			processed.Add(1)
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitFor(t, func() bool { return processed.Load() == n }, 5*time.Second)
	cancel()
	require.NoError(t, <-done)

	require.Equal(t, int64(n), processed.Load())
	st, err := m.Stats(context.Background())
	require.NoError(t, err)
	require.Equal(t, jobq.Stats{Done: n}, st)
}

func TestWorker_FailsOnHandlerError(t *testing.T) {
	m := memq.New(3)
	_, _ = m.Enqueue(context.Background(), "f", nil)

	w := worker.New(m, worker.Config{Workers: 1, PollInterval: 5 * time.Millisecond},
		func(context.Context, *jobq.Job) error { return errors.New("boom") })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitFor(t, func() bool { return m.CountByStatus(jobq.Failed) == 1 }, 5*time.Second)
	cancel()
	require.NoError(t, <-done)

	j := m.GetByKey("f")
	require.Equal(t, jobq.Failed, j.Status)
	require.Equal(t, "boom", j.Error)
}

func TestWorker_RecoversPanic(t *testing.T) {
	m := memq.New(3)
	_, _ = m.Enqueue(context.Background(), "p1", nil)
	_, _ = m.Enqueue(context.Background(), "p2", nil)

	w := worker.New(m, worker.Config{Workers: 1, PollInterval: 5 * time.Millisecond},
		func(_ context.Context, job *jobq.Job) error {
			if job.Key == "p1" {
				panic("kaboom")
			}
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitFor(t, func() bool { return m.CountByStatus(jobq.Done)+m.CountByStatus(jobq.Failed) == 2 }, 5*time.Second)
	cancel()
	require.NoError(t, <-done)

	require.Equal(t, jobq.Failed, m.GetByKey("p1").Status)
	require.Contains(t, m.GetByKey("p1").Error, "panic: kaboom")
	require.Equal(t, jobq.Done, m.GetByKey("p2").Status)
}

func TestWorker_GracefulShutdown(t *testing.T) {
	m := memq.New(3)
	_, _ = m.Enqueue(context.Background(), "slow", nil)

	started := make(chan struct{})
	finish := make(chan struct{})
	w := worker.New(m, worker.Config{Workers: 1, PollInterval: 5 * time.Millisecond, ShutdownTimeout: 2 * time.Second},
		func(context.Context, *jobq.Job) error {
			close(started)
			<-finish
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	<-started
	cancel()      // request shutdown while the handler is running
	close(finish) // handler finishes successfully within the grace period
	require.NoError(t, <-done)

	require.Equal(t, jobq.Done, m.GetByKey("slow").Status)
}

func TestWorker_AbandonsSlowHandlerOnShutdown(t *testing.T) {
	m := memq.New(3)
	m.SetLease(time.Millisecond) // so a later ReclaimStale picks the job up immediately
	_, _ = m.Enqueue(context.Background(), "slow", nil)

	started := make(chan struct{})
	w := worker.New(m, worker.Config{Workers: 1, PollInterval: 5 * time.Millisecond, ShutdownTimeout: 50 * time.Millisecond},
		func(ctx context.Context, _ *jobq.Job) error {
			close(started)
			<-ctx.Done()
			time.Sleep(200 * time.Millisecond) // outlives ShutdownTimeout
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	<-started
	cancel()
	start := time.Now()
	require.NoError(t, <-done)
	require.Less(t, time.Since(start), time.Second, "Run must not wait for the abandoned handler")

	// The job is left processing; a later ReclaimStale picks it up.
	require.Equal(t, jobq.Processing, m.GetByKey("slow").Status)
	_, _ = m.ReclaimStale(context.Background(), 0)
	require.Equal(t, jobq.Pending, m.GetByKey("slow").Status)
}

func TestWorker_HeartbeatKeepsLongJob(t *testing.T) {
	m := memq.New(3, 50*time.Millisecond) // staleAfter
	m.SetLease(150 * time.Millisecond)
	_, _ = m.Enqueue(context.Background(), "long", nil)

	done := make(chan struct{})
	w := worker.New(m, worker.Config{
		Workers:           1,
		PollInterval:      5 * time.Millisecond,
		ReclaimInterval:   20 * time.Millisecond,
		HeartbeatInterval: 30 * time.Millisecond,
	}, func(context.Context, *jobq.Job) error {
		time.Sleep(200 * time.Millisecond) // outlives lease+staleAfter
		close(done)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- w.Run(ctx) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("long handler never finished")
	}
	cancel()
	require.NoError(t, <-runDone)

	// The reclaim loop must not have stolen the job thanks to heartbeats.
	require.Equal(t, jobq.Done, m.GetByKey("long").Status)
}

func TestWorker_NilHandler(t *testing.T) {
	m := memq.New(3)
	w := worker.New(m, worker.Config{}, nil)
	require.Error(t, w.Run(context.Background()))
}

// TestWorker_CancelsHandlerOnLostLease verifies that when heartbeats stop
// being accepted (the lease lapsed), the worker cancels the in-flight
// handler instead of silently continuing on a job that may be re-claimed.
func TestWorker_CancelsHandlerOnLostLease(t *testing.T) {
	m := memq.New(3)
	m.SetLease(20 * time.Millisecond)
	_, _ = m.Enqueue(context.Background(), "lease-lost", nil)

	handlerCanceled := make(chan struct{})
	w := worker.New(m, worker.Config{
		Workers:           1,
		PollInterval:      5 * time.Millisecond,
		HeartbeatInterval: 100 * time.Millisecond, // slower than the lease: lost
	}, func(ctx context.Context, _ *jobq.Job) error {
		<-ctx.Done()
		close(handlerCanceled)
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case <-handlerCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("handler was not cancelled after lease loss")
	}
	cancel()
	require.NoError(t, <-done)
}
