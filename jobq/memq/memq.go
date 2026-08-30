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
	}
}

// MaxAttempts returns the configured retry limit (useful in test assertions).
func (m *Mem) MaxAttempts() int { return m.maxAttempts }

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
		CreatedAt: time.Now().UTC(),
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
		if target == nil || j.ID < target.ID {
			target = j
		}
	}

	if target == nil {
		return nil, jobq.ErrNoJobs
	}

	now := time.Now().UTC()
	target.Status = jobq.Processing
	target.Attempts++
	target.StartedAt = &now

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

	now := time.Now().UTC()
	j.Status = jobq.Done
	j.FinishedAt = &now // see note below

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

	if j.Attempts >= m.maxAttempts {
		now := time.Now().UTC()
		j.Status = jobq.Failed
		j.FinishedAt = &now
	} else {
		j.Status = jobq.Pending
		j.FinishedAt = nil
	}

	return nil
}

// ReclaimStale returns processing jobs whose StartedAt is older than timeout
// back to pending. Returns the number of reclaimed jobs.
// A timeout <= 0 falls back to the queue's configured staleAfter (5 minutes
// by default), matching pgq semantics.
func (m *Mem) ReclaimStale(_ context.Context, timeout time.Duration) (int64, error) {
	if timeout <= 0 {
		timeout = m.staleAfter
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().UTC().Add(-timeout)
	var count int64

	for _, j := range m.jobs {
		if j.Status != jobq.Processing {
			continue
		}
		if j.StartedAt == nil {
			continue
		}
		if j.StartedAt.Before(cutoff) {
			j.Status = jobq.Pending
			j.StartedAt = nil
			j.Error = ""
			count++
		}
	}

	return count, nil
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
