-- 074: the structured result of a background job's last run (WP-R7-5).
--
-- job_runs.last_error is the run's note — text for a human. Jobs whose outcome
-- is a set of counts (the routing-truth check: per backend present / missing /
-- error / unknown-backend rows) write them here as JSON, so that the admin API,
-- the dashboard's System page and the Prometheus collector read the LAST RUN
-- from the table — right after a restart too — instead of an in-process value
-- that reads 0 until the job runs again (the Review R13-14 lesson).
ALTER TABLE job_runs ADD COLUMN IF NOT EXISTS result JSONB;
