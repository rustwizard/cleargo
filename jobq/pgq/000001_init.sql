-- 000001_init.sql
-- Core jobq queue table.

CREATE TABLE IF NOT EXISTS jobq_jobs (
    id           bigserial   PRIMARY KEY,
    key          text        NOT NULL UNIQUE,
    payload      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    status       text        NOT NULL DEFAULT 'pending'
                 CONSTRAINT jobq_jobs_status_chk
                     CHECK (status IN ('pending', 'processing', 'done', 'failed')),
    attempts     integer     NOT NULL DEFAULT 0,
    max_attempts integer     NOT NULL,
    error        text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    started_at   timestamptz,
    finished_at  timestamptz
);

-- Claim scans pending jobs ordered by id; a partial index keeps that cheap.
CREATE INDEX IF NOT EXISTS jobq_jobs_pending_idx
    ON jobq_jobs (id)
    WHERE status = 'pending';
