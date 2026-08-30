-- 000002_backoff.sql
-- Defer retries with an exponential backoff.
--
-- run_after holds the earliest moment a pending job may be claimed again.
-- Claim only picks jobs with run_after <= now(), and Fail schedules the
-- next retry as now() + RetryBase * 2^(attempts-1), capped at RetryCap.

ALTER TABLE jobq_jobs
    ADD COLUMN IF NOT EXISTS run_after timestamptz NOT NULL DEFAULT now();
