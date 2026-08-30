// Package mem provides an in-memory implementation of jobq.Queue for
// unit-testing worker logic without a running database.
//
// It is NOT intended for production use. Concurrency is guarded by a
// mutex, but there is no persistence, no TTL, and no observability.
package memq

import (
	"context"
	"errors"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rustwizard/cleargo/jobq"
)

// ErrNotProcessing mirrors postgres.ErrNotProcessing: returned by Ack and
// Fail when the target job is not in the "processing" state.
var ErrNotProcessing = errors.New("memq: job is not in processing state")

// Mem is a thread-safe, in-memory jobq.Queue.
type Mem struct {
	mu          sync.Mutex
	jobs        map[int64]*jobq.Job
	keys        map[string]int64 // key → id, for idempotency
	nextID      int64
	maxAttempts int
	staleAfter  time.Duration
	retryBase   time.Duration       // <= 0 disables backoff
	retryCap    time.Duration       // <= 0 means no cap
	retryAt     map[int64]time.Time // id → earliest claim time after a failed attempt
	lease       time.Duration       // lease duration set at claim time (default 5m)
	leaseUntil  map[int64]time.Time // id → lease expiry, while processing
	nowFn       func() time.Time    // injectable clock, default time.Now
}

// New creates an in-memory queue. maxAttempts is stamped onto every job
// at enqueue time, matching the Postgres implementation. A value <= 0
// defaults to 3.
//
// Optional staleAfter overrides the default orphan timeout used by
// ReclaimStale when called with timeout <= 0 (default: 5 minutes),
// matching pgq.Config.StaleAfter semantics.
func New(maxAttempts int, staleAfter ...time.Duration) *Mem {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	sa := 5 * time.Minute
	if len(staleAfter) > 0 && staleAfter[0] > 0 {
		sa = staleAfter[0]
	}
	return &Mem{
		jobs:        make(map[int64]*jobq.Job),
		keys:        make(map[string]int64),
		nextID:      1,
		maxAttempts: maxAttempts,
		staleAfter:  sa,
		retryAt:     make(map[int64]time.Time),
		lease:       5 * time.Minute,
		leaseUntil:  make(map[int64]time.Time),
		nowFn:       time.Now,
	}
}

// SetClock overrides the queue's time source (default time.Now), which makes
// backoff/lease/reclaim behaviour deterministic in tests: advance the clock
// instead of sleeping. Passing nil restores the real clock.
func (m *Mem) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	m.nowFn = now
}

// now returns the current time (UTC) according to the injected clock.
func (m *Mem) now() time.Time { return m.nowFn().UTC() }

// MaxAttempts returns the configured retry limit (useful in test assertions).
func (m *Mem) MaxAttempts() int { return m.maxAttempts }

// SetRetryBase enables the exponential retry backoff: after a non-terminal
// Fail the job becomes claimable again after RetryBase * 2^(attempts-1).
// A value <= 0 (default) makes failed jobs immediately claimable.
func (m *Mem) SetRetryBase(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retryBase = d
}

// SetRetryCap caps the exponential backoff delay computed from RetryBase,
// matching pgq.Config.RetryCap. A value <= 0 (default) leaves the delay
// uncapped.
func (m *Mem) SetRetryCap(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retryCap = d
}

// SetLease sets how long a claimed job's lease runs (default 5 minutes),
// matching pgq.Config.Lease semantics. Workers processing long jobs must
// call Heartbeat to renew it; otherwise ReclaimStale reclaims the job once
// the lease expires. A value <= 0 is ignored.
func (m *Mem) SetLease(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d > 0 {
		m.lease = d
	}
}

// ---------------------------------------------------------------------------
// jobq.Queue
// ---------------------------------------------------------------------------

// Enqueue adds a job. Idempotent by key: if the key already exists (in any
// status), nothing is written and false is returned.
func (m *Mem) Enqueue(_ context.Context, key string, payload map[string]any) (bool, error) {
	if key == "" {
		return false, errors.New("memq: enqueue: empty key")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.keys[key]; exists {
		return false, nil
	}

	id := m.nextID
	m.nextID++

	m.jobs[id] = &jobq.Job{
		ID:        id,
		Key:       key,
		Payload:   payload,
		Status:    jobq.Pending,
		Attempts:  0,
		CreatedAt: m.now(),
	}
	m.keys[key] = id

	return true, nil
}

// Claim picks the lowest-ID pending job (matching Postgres ORDER BY id ASC),
// transitions it to processing, increments attempts, and returns a copy.
// Returns jobq.ErrNoJobs if the queue is empty or all pending jobs have
// exhausted their attempts.
func (m *Mem) Claim(_ context.Context) (*jobq.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Linear scan for lowest ID — O(n), fine for test-scale queues.
	var target *jobq.Job
	for id, j := range m.jobs {
		_ = id
		if j.Status != jobq.Pending {
			continue
		}
		if j.Attempts >= m.maxAttempts {
			continue
		}
		if ra, ok := m.retryAt[j.ID]; ok && ra.After(m.now()) {
			continue // still in backoff
		}
		if target == nil || j.ID < target.ID {
			target = j
		}
	}

	if target == nil {
		return nil, jobq.ErrNoJobs
	}

	now := m.now()
	target.Status = jobq.Processing
	target.Attempts++
	target.StartedAt = &now
	delete(m.retryAt, target.ID)
	m.leaseUntil[target.ID] = now.Add(m.lease) // claim grants a renewable lease

	// Shallow copy: caller gets an independent struct, but Payload map is
	// shared. Acceptable for a test helper.
	cp := *target
	return &cp, nil
}

// Ack transitions a processing job to done.
// Returns ErrNotProcessing if the job is not currently in processing state.
func (m *Mem) Ack(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	j, ok := m.jobs[id]
	if !ok {
		return errors.New("memq: ack: job not found")
	}
	if j.Status != jobq.Processing {
		return ErrNotProcessing
	}

	now := m.now()
	j.Status = jobq.Done
	j.FinishedAt = &now // see note below
	delete(m.retryAt, id)
	delete(m.leaseUntil, id) // done jobs keep no lease

	return nil
}

// Fail records an error. If attempts < maxAttempts the job returns to
// pending for retry; otherwise it transitions to the terminal "failed" state.
// Returns ErrNotProcessing if the job is not currently in processing state.
// The error message is truncated to 2000 runes, matching pgq.
func (m *Mem) Fail(_ context.Context, id int64, errMsg string) error {
	if utf8.RuneCountInString(errMsg) > 2000 {
		errMsg = string([]rune(errMsg)[:2000])
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	j, ok := m.jobs[id]
	if !ok {
		return errors.New("memq: fail: job not found")
	}
	if j.Status != jobq.Processing {
		return ErrNotProcessing
	}

	j.Error = errMsg
	j.StartedAt = nil
	delete(m.leaseUntil, id) // no longer processing: keep no lease

	if j.Attempts >= m.maxAttempts {
		now := m.now()
		j.Status = jobq.Failed
		j.FinishedAt = &now
		delete(m.retryAt, id)
	} else {
		j.Status = jobq.Pending
		j.FinishedAt = nil
		if m.retryBase > 0 {
			m.retryAt[id] = m.now().Add(m.backoffDelay(j.Attempts))
		} else {
			delete(m.retryAt, id)
		}
	}

	return nil
}

// backoffDelay returns RetryBase * 2^(attempts-1), capped at RetryCap
// (when set), mirroring pgq's exponential backoff.
func (m *Mem) backoffDelay(attempts int) time.Duration {
	exp := attempts - 1
	if exp > 30 { // avoid overflow; a cap (if any) clamps it anyway
		exp = 30
	}
	delay := m.retryBase * time.Duration(1<<uint(exp))
	if m.retryCap > 0 && delay > m.retryCap {
		delay = m.retryCap
	}
	return delay
}

// Heartbeat renews the job's lease so ReclaimStale does not treat a live,
// long-running worker as crashed. Returns ErrNotProcessing if the job is not
// processing or its lease has already expired (in which case worker A must
// win the race: re-running a job whose lease lapsed is intended).
func (m *Mem) Heartbeat(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	j, ok := m.jobs[id]
	if !ok {
		return errors.New("memq: heartbeat: job not found")
	}
	if j.Status != jobq.Processing {
		return ErrNotProcessing
	}

	now := m.now()
	if until, ok := m.leaseUntil[id]; ok && !until.After(now) {
		return ErrNotProcessing // lease already expired → job may be reclaimed
	}

	m.leaseUntil[id] = now.Add(m.lease) // renewable: see pgq Heartbeat
	return nil
}

// ReclaimStale returns orphaned jobs back to pending and reports how many. A
// job is orphaned when its claim lease has expired; jobs without a recorded
// expiry (legacy state) fall back to the StartedAt-based timeout. A timeout
// <= 0 falls back to the queue's configured staleAfter (5 minutes by
// default), matching pgq semantics.
func (m *Mem) ReclaimStale(_ context.Context, timeout time.Duration) (int64, error) {
	if timeout <= 0 {
		timeout = m.staleAfter
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	cutoff := now.Add(-timeout) // legacy fallback horizon only
	var count int64

	for _, j := range m.jobs {
		if j.Status != jobq.Processing {
			continue
		}

		stale := false
		if until, ok := m.leaseUntil[j.ID]; ok {
			stale = !until.After(now) // lease expired → orphaned, like pgq
		} else if j.StartedAt != nil { // legacy: no lease recorded
			stale = j.StartedAt.Before(cutoff)
		}

		if !stale {
			continue
		}

		j.Status = jobq.Pending
		j.StartedAt = nil
		j.Error = ""

		delete(m.retryAt, j.ID) // reclaimed jobs become claimable immediately
		delete(m.leaseUntil, j.ID)
		count++
	}

	return count, nil
}

// Stats returns the number of jobs per status.
func (m *Mem) Stats(_ context.Context) (jobq.Stats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var s jobq.Stats
	for _, j := range m.jobs {
		switch j.Status {
		case jobq.Pending:
			s.Pending++
		case jobq.Processing:
			s.Processing++
		case jobq.Done:
			s.Done++
		case jobq.Failed:
			s.Failed++
		}
	}
	return s, nil
}

// Depth returns the number of jobs Claim can hand out right now
// (pending, not yet exhausted their attempts, and past their backoff),
// matching Claim's logic.
func (m *Mem) Depth(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	var n int
	for _, j := range m.jobs {
		if j.Status != jobq.Pending || j.Attempts >= m.maxAttempts {
			continue
		}
		if ra, ok := m.retryAt[j.ID]; ok && ra.After(now) {
			continue
		}
		n++
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Test helpers (not part of jobq.Queue)
// ---------------------------------------------------------------------------

// Len returns the total number of jobs in all states. Useful in test
// assertions: mem.Len() == 3.
func (m *Mem) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.jobs)
}

// CountByStatus returns the number of jobs in a given status.
func (m *Mem) CountByStatus(status jobq.Status) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int
	for _, j := range m.jobs {
		if j.Status == status {
			n++
		}
	}
	return n
}

// Get returns a copy of the job with the given ID, or nil if not found.
// Useful for asserting internal state in tests.
func (m *Mem) Get(id int64) *jobq.Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil
	}
	cp := *j
	return &cp
}

// GetByKey returns a copy of the job with the given key, or nil.
func (m *Mem) GetByKey(key string) *jobq.Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.keys[key]
	if !ok {
		return nil
	}
	j := m.jobs[id]
	cp := *j
	return &cp
}
