CREATE TABLE IF NOT EXISTS relay_runtime_sessions (
    tenant_id TEXT NOT NULL DEFAULT '',
    project_id TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    runtime_id TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL,
    native_session_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, project_id, session_id, agent_id, runtime_id, provider)
);
