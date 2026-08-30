// Package worker runs a jobq.Queue with a pool of goroutines: it claims
// jobs, executes a handler, and settles them (ack on success, fail on error
// or panic). It can also run a periodic ReclaimStale loop and keep long
// jobs alive with automatic heartbeats.
//
// The worker is deliberately thin: it owns the process lifecycle (claim →
// handle → settle), while retries, backoff and leases stay the queue's job.
package worker

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/rustwizard/cleargo/jobq"
)

// Handler processes one claimed job. Returning an error marks the job failed;
// returning nil acks it. A panic is recovered and treated as a failure.
type Handler func(ctx context.Context, job *jobq.Job) error

// Config tunes the worker. Zero values get defaults.
type Config struct {
	// Workers is the number of concurrent claim/process goroutines.
	// Default: runtime.NumCPU().
	Workers int

	// PollInterval is how long a worker sleeps when the queue is empty
	// (ErrNoJobs) or on transient errors, instead of hot-looping.
	// Default: 100ms.
	PollInterval time.Duration

	// ReclaimInterval is how often the worker runs ReclaimStale.
	// 0 (default) disables the reclaim loop.
	ReclaimInterval time.Duration

	// ReclaimTimeout is passed to ReclaimStale (0 = queue default).
	ReclaimTimeout time.Duration

	// HeartbeatInterval is how often the worker renews the lease of the
	// job it is currently processing (only if the queue implements
	// jobq.Heartbeater). 0 (default) disables automatic heartbeats.
	HeartbeatInterval time.Duration

	// ShutdownTimeout is how long Run waits for in-flight handlers to
	// finish after ctx is cancelled before abandoning them (they stay in
	// "processing" and will be reclaimed). Default: 30s.
	ShutdownTimeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.Workers <= 0 {
		c.Workers = runtime.NumCPU()
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 100 * time.Millisecond
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = 30 * time.Second
	}
	return c
}

// Worker claims jobs from a jobq.Queue and settles them with a handler.
type Worker struct {
	q       jobq.Queue
	handler Handler
	cfg     Config

	mu     sync.Mutex
	active map[int64]context.CancelFunc // in-flight jobs, for graceful shutdown
}

// New creates a worker for q. handler may be nil; Run returns an error then.
func New(q jobq.Queue, cfg Config, handler Handler) *Worker {
	return &Worker{
		q:       q,
		handler: handler,
		cfg:     cfg.withDefaults(),
		active:  make(map[int64]context.CancelFunc),
	}
}

// Run starts cfg.Workers processing goroutines (plus an optional reclaim
// loop) and blocks until ctx is cancelled. On cancellation the workers stop
// claiming new jobs and give in-flight handlers ShutdownTimeout to finish
// before Run returns.
func (w *Worker) Run(ctx context.Context) error {
	if w.handler == nil {
		return errors.New("worker: handler is nil")
	}

	var wg sync.WaitGroup
	for i := 0; i < w.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.runWorker(ctx)
		}()
	}
	if w.cfg.ReclaimInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.runReclaim(ctx)
		}()
	}

	wg.Wait()
	return nil
}

// runWorker claims and processes jobs until ctx is done.
func (w *Worker) runWorker(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := w.q.Claim(ctx)
		if err != nil {
			if !sleep(ctx, w.cfg.PollInterval) {
				return
			}
			continue
		}
		w.process(ctx, job)
	}
}

// process runs the handler for one job, heartbeating it while it runs, and
// settles it with ack/fail. On shutdown the handler gets ShutdownTimeout to
// finish; otherwise the job is left "processing" for ReclaimStale.
func (w *Worker) process(parent context.Context, job *jobq.Job) {
	jctx, cancel := context.WithCancel(parent)
	w.track(job.ID, cancel)
	defer func() {
		w.untrack(job.ID)
		cancel()
	}()

	if w.cfg.HeartbeatInterval > 0 {
		hbCtx, hbCancel := context.WithCancel(jctx)
		defer hbCancel()
		go w.heartbeatLoop(hbCtx, w.q.Heartbeat, job.ID, cancel)
	}

	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				runErr = fmt.Errorf("worker: handler panic: %v", r)
			}
		}()
		runErr = w.handler(jctx, job)
	}()

	select {
	case <-done:
		// handler finished on its own
	case <-parent.Done():
		// shutdown requested: give the handler a grace period
		select {
		case <-done:
		case <-time.After(w.cfg.ShutdownTimeout):
			return // abandon; ReclaimStale will pick the job up later
		}
	}

	// Settle with a fresh context so a cancelled parent does not block ack/fail.
	if runErr != nil {
		_ = w.q.Fail(context.Background(), job.ID, runErr.Error())
	} else {
		_ = w.q.Ack(context.Background(), job.ID)
	}
}

// heartbeatLoop renews the job's lease until the job finishes or the lease is
// lost (Heartbeat returns an error) — in which case the handler is cancelled.
func (w *Worker) heartbeatLoop(ctx context.Context, heartbeat func(context.Context, int64) error, id int64, cancel context.CancelFunc) {
	ticker := time.NewTicker(w.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := heartbeat(ctx, id); err != nil {
				cancel() // lease lost: stop the handler, job may be reclaimed
				return
			}
		}
	}
}

// runReclaim periodically reclaims orphaned jobs.
func (w *Worker) runReclaim(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.ReclaimInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = w.q.ReclaimStale(ctx, w.cfg.ReclaimTimeout)
		}
	}
}

func (w *Worker) track(id int64, cancel context.CancelFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.active[id] = cancel
}

func (w *Worker) untrack(id int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.active, id)
}

// sleep waits for d or ctx cancellation, returning false when cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
