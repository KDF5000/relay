-- Usage remains in the durable event log; only the latest absolute snapshot
-- for each execution attempt is needed by Run queries.
CREATE INDEX IF NOT EXISTS relay_events_usage_attempt_idx
  ON relay_events (run_id, attempt_id, sequence DESC) WHERE type='usage.updated';
