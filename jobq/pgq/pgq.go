// Package pgq implements jobq.Queue on top of PostgreSQL using
// SELECT ... FOR UPDATE SKIP LOCKED for concurrent-safe claiming.
//
// The target table must exist before calling New(). Apply the embedded
// migrations first:
//
//	_, err := pgq.Migrate(ctx, dsn, 30*time.Second)
//
// The module does NOT auto-migrate on New().
package pgq

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rustwizard/cleargo/jobq"
)

// DB is the minimal interface satisfied by *sql.DB (and compatible
// database/sql drivers, e.g. pgx stdlib). pgxpool.Pool needs a thin adapter
// because its methods return pgx-native types.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Config holds tunables for the Postgres queue. Zero values get defaults.
type Config struct {
	// Table is the target table name. Must match [a-z_][a-z0-9_]*.
	// Default: "jobq_jobs".
	Table string

	// MaxAttempts is the default retry limit stamped onto new jobs at enqueue time.
	// Default: 3.
	MaxAttempts int

	// StaleAfter is the default timeout for ReclaimStale.
	// A job stuck in "processing" longer than this is considered orphaned.
	// Default: 5 * time.Minute.
	StaleAfter time.Duration

	// Metrics is a Prometheus registerer for queue metrics. When non-nil the
	// queue registers a collector exposing:
	//
	//	jobq_ops_total{table,op}            — counter of operations
	//	jobq_jobs_by_status{table,status}   — gauge, refreshed on scrape
	//
	// All queues sharing one registerer must use distinct tables; the table
	// label keeps their series apart. Nil disables metrics.
	Metrics prometheus.Registerer
}

func (c Config) withDefaults() Config {
	if c.Table == "" {
		c.Table = "jobq_jobs"
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = 5 * time.Minute
	}
	return c
}

// Postgres is a jobq.Queue backed by a single PostgreSQL table.
type Postgres struct {
	db          DB
	table       string // validated, safe to interpolate
	maxAttempts int
	staleAfter  time.Duration
	metrics     *queueMetrics // nil when metrics are disabled
}

// New creates a Postgres queue. It does NOT verify that the table exists;
// call Migrate or ensure your migration has run before first use.
func New(db DB, cfg Config) (*Postgres, error) {
	if db == nil {
		return nil, errors.New("pgq: db must not be nil")
	}

	cfg = cfg.withDefaults()

	if err := validateTableName(cfg.Table); err != nil {
		return nil, fmt.Errorf("pgq: %w", err)
	}

	q := &Postgres{
		db:          db,
		table:       cfg.Table,
		maxAttempts: cfg.MaxAttempts,
		staleAfter:  cfg.StaleAfter,
	}

	if cfg.Metrics != nil {
		m, err := registerQueueMetrics(cfg.Metrics, q)
		if err != nil {
			return nil, fmt.Errorf("pgq: register metrics: %w", err)
		}
		q.metrics = m
	}

	return q, nil
}

// StaleAfter returns the configured orphan timeout. Useful for callers that
// run a separate reclaim loop and want to stay in sync with the config.
func (p *Postgres) StaleAfter() time.Duration { return p.staleAfter }

// ---------------------------------------------------------------------------
// jobq.Queue implementation
// ---------------------------------------------------------------------------

// Enqueue inserts a new job. It is idempotent: if a row with the same key
// already exists (regardless of status), nothing is written and false is
// returned. The caller decides whether "already exists" is an error or a no-op.
func (p *Postgres) Enqueue(ctx context.Context, key string, payload map[string]any) (bool, error) {
	if key == "" {
		return false, errors.New("pgq: enqueue: empty key")
	}

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("pgq: enqueue: marshal payload: %w", err)
	}

	res, err := p.db.ExecContext(ctx, `
		INSERT INTO `+p.table+` (key, payload, max_attempts)
		VALUES ($1, $2::jsonb, $3)
		ON CONFLICT (key) DO NOTHING
	`, key, payloadJSON, p.maxAttempts)
	if err != nil {
		return false, fmt.Errorf("pgq: enqueue: %w", err)
	}

	n, _ := res.RowsAffected()
	if n > 0 {
		p.incOp("enqueue")
	}
	return n > 0, nil
}

// Claim atomically picks one pending job and transitions it to processing.
// It uses FOR UPDATE SKIP LOCKED so multiple workers can claim concurrently
// without blocking each other. Returns jobq.ErrNoJobs when the queue is empty
// or all pending jobs have exhausted their attempts.
func (p *Postgres) Claim(ctx context.Context) (*jobq.Job, error) {
	var (
		id           int64
		key          string
		payloadBytes []byte
		status       jobq.Status
		attempts     int
		createdAt    time.Time
		startedAt    sql.NullTime
	)

	err := p.db.QueryRowContext(ctx, `
		UPDATE `+p.table+`
		SET status     = 'processing',
		    started_at = now(),
		    attempts   = attempts + 1,
		    error      = NULL
		WHERE id = (
			SELECT id FROM `+p.table+`
			WHERE status = 'pending'
			  AND attempts < max_attempts
			ORDER BY id ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, key, payload, status, attempts, created_at, started_at
	`).Scan(&id, &key, &payloadBytes, &status, &attempts, &createdAt, &startedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, jobq.ErrNoJobs
	}
	if err != nil {
		return nil, fmt.Errorf("pgq: claim: %w", err)
	}
	p.incOp("claim")

	var payload map[string]any
	if len(payloadBytes) > 0 {
		if err := json.Unmarshal(payloadBytes, &payload); err != nil {
			return nil, fmt.Errorf("pgq: claim: unmarshal payload for job %d: %w", id, err)
		}
	}

	job := &jobq.Job{
		ID:        id,
		Key:       key,
		Payload:   payload,
		Status:    status,
		Attempts:  attempts,
		CreatedAt: createdAt,
	}
	if startedAt.Valid {
		job.StartedAt = &startedAt.Time
	}

	return job, nil
}

// Ack marks a job as successfully completed.
func (p *Postgres) Ack(ctx context.Context, id int64) error {
	res, err := p.db.ExecContext(ctx, `
		UPDATE `+p.table+`
		SET status = 'done', finished_at = now()
		WHERE id = $1 AND status = 'processing'
	`, id)
	if err != nil {
		return fmt.Errorf("pgq: ack job %d: %w", id, err)
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("pgq: ack job %d: %w", id, ErrNotProcessing)
	}
	p.incOp("ack")
	return nil
}

// Fail records an error on a job. If the job has not yet exhausted its
// max_attempts it is put back to pending for retry; otherwise it transitions
// to the terminal "failed" state.
//
// The error message is truncated to 2000 characters to keep the table tidy.
// Truncation is rune-safe so multibyte (e.g. Cyrillic) text is never cut mid-rune.
func (p *Postgres) Fail(ctx context.Context, id int64, errMsg string) error {
	if utf8.RuneCountInString(errMsg) > 2000 {
		errMsg = string([]rune(errMsg)[:2000])
	}

	res, err := p.db.ExecContext(ctx, `
		UPDATE `+p.table+`
		SET status      = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
		    error       = $2,
		    started_at  = NULL,
		    finished_at = CASE WHEN attempts >= max_attempts THEN now() ELSE NULL END
		WHERE id = $1 AND status = 'processing'
	`, id, errMsg)
	if err != nil {
		return fmt.Errorf("pgq: fail job %d: %w", id, err)
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("pgq: fail job %d: %w", id, ErrNotProcessing)
	}
	p.incOp("fail")
	return nil
}

// ReclaimStale returns orphaned jobs (stuck in "processing" longer than the
// given timeout) back to "pending" so another worker can pick them up.
// This handles the case where a worker crashed, was OOM-killed, or lost
// network connectivity mid-processing.
//
// The timeout parameter overrides the config's StaleAfter for this call.
// Pass time.Duration(0) to use the configured default.
//
// Returns the number of jobs reclaimed.
func (p *Postgres) ReclaimStale(ctx context.Context, timeout time.Duration) (int64, error) {
	if timeout <= 0 {
		timeout = p.staleAfter
	}

	res, err := p.db.ExecContext(ctx, `
		UPDATE `+p.table+`
		SET status     = 'pending',
		    started_at = NULL,
		    error      = NULL
		WHERE status = 'processing'
		  AND started_at < now() - make_interval(secs => $1)
	`, float64(timeout.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("pgq: reclaim stale: %w", err)
	}

	n, _ := res.RowsAffected()
	if n > 0 {
		p.incOp("reclaim")
	}
	return n, nil
}

// Stats returns the number of jobs per status at the moment of the query.
func (p *Postgres) Stats(ctx context.Context) (jobq.Stats, error) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT status, count(*) FROM `+p.table+` GROUP BY status
	`)
	if err != nil {
		return jobq.Stats{}, fmt.Errorf("pgq: stats: %w", err)
	}
	defer rows.Close()

	var s jobq.Stats
	for rows.Next() {
		var st jobq.Status
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return jobq.Stats{}, fmt.Errorf("pgq: stats: %w", err)
		}
		switch st {
		case jobq.Pending:
			s.Pending = n
		case jobq.Processing:
			s.Processing = n
		case jobq.Done:
			s.Done = n
		case jobq.Failed:
			s.Failed = n
		}
	}
	if err := rows.Err(); err != nil {
		return jobq.Stats{}, fmt.Errorf("pgq: stats: %w", err)
	}
	return s, nil
}

// Depth returns the number of jobs Claim can hand out right now
// (pending and not yet exhausted their attempts).
func (p *Postgres) Depth(ctx context.Context) (int, error) {
	var n int
	err := p.db.QueryRowContext(ctx, `
		SELECT count(*) FROM `+p.table+`
		WHERE status = 'pending' AND attempts < max_attempts
	`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("pgq: depth: %w", err)
	}
	return n, nil
}

// incOp bumps the per-table operation counter when metrics are enabled.
func (p *Postgres) incOp(op string) {
	if p.metrics != nil {
		p.metrics.opsTotal.WithLabelValues(p.table, op).Inc()
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// ErrNotProcessing is returned by Ack and Fail when the target job is not
// currently in the "processing" state. This typically means the job was
// reclaimed by a stale-reclaim while the original worker was still running,
// or was already completed by another worker.
var ErrNotProcessing = errors.New("pgq: job is not in processing state")

func validateTableName(name string) error {
	if name == "" {
		return errors.New("table name must not be empty")
	}
	// PostgreSQL limits identifiers to 63 *characters*, not bytes.
	if utf8.RuneCountInString(name) > 63 {
		return fmt.Errorf("table name %q exceeds 63 characters", name)
	}
	for i, c := range name {
		switch {
		case i == 0:
			if !((c >= 'a' && c <= 'z') || c == '_') {
				return fmt.Errorf("table name %q: first character must be [a-z_]", name)
			}
		default:
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
				return fmt.Errorf("table name %q: only [a-z0-9_] allowed at position %d", name, i)
			}
		}
	}
	return nil
}
