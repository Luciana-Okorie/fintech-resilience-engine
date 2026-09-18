-- Day 12: Fintech Dependency Risk & Resilience Engine
-- Initial schema: dependency registry + incident history.

CREATE TABLE IF NOT EXISTS dependencies (
    service     TEXT PRIMARY KEY,
    depends_on  JSONB NOT NULL DEFAULT '[]',
    criticality INT NOT NULL DEFAULT 1,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS incidents (
    id                TEXT PRIMARY KEY,
    severity          TEXT NOT NULL,
    dependency        TEXT NOT NULL,
    failure_reason    TEXT NOT NULL,
    affected_services JSONB NOT NULL DEFAULT '[]',
    current_action    TEXT,
    fallback          TEXT,
    status            TEXT NOT NULL,
    started_at        TIMESTAMPTZ NOT NULL,
    resolved_at       TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_incidents_dependency_started
    ON incidents (dependency, started_at DESC);

CREATE INDEX IF NOT EXISTS idx_incidents_status
    ON incidents (status);
