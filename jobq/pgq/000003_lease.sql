-- 000003_lease.sql
-- Give claimed jobs a renewable lease so ReclaimStale does not steal
-- long-running jobs from live workers.
--
-- Claim sets lease_until = now() + Lease; Heartbeat renews it; ReclaimStale
-- only reclaims jobs whose lease has expired (lease_until < now()).

ALTER TABLE jobq_jobs
    ADD COLUMN IF NOT EXISTS lease_until timestamptz;
