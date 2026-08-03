package workload

// FixtureSchema is a test-only mirror of the durable control-plane tables the
// workload path validates against (pools/turns/runs/assignments), NOT the
// control-plane's actual migrations — so gateway tests stay decoupled from the
// control-plane repo (B+C approach, mirroring snapshot.FixtureSchema).
// Exported so the server integration test can reuse it.
const FixtureSchema = `
CREATE SCHEMA IF NOT EXISTS toolgateway;
CREATE SCHEMA IF NOT EXISTS runtime;

CREATE TABLE toolgateway.pools (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key              text NOT NULL UNIQUE,
    name             text NOT NULL,
    spiffe_id_prefix text NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    deleted_at       timestamptz
);

CREATE TABLE runtime.workflow_runs (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind              text NOT NULL,
    definition_key    text,
    scope_identity_id uuid NOT NULL,
    session_id        text NOT NULL,
    session_dir       text NOT NULL,
    trigger           jsonb NOT NULL DEFAULT '{}'::jsonb,
    state             text NOT NULL DEFAULT 'pending',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    started_at        timestamptz,
    finished_at       timestamptz
);

CREATE TABLE runtime.turns (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id     uuid NOT NULL REFERENCES runtime.workflow_runs(id) ON DELETE CASCADE,
    step_id    uuid,
    session_id text NOT NULL,
    model      text,
    state      text NOT NULL DEFAULT 'pending' CHECK (state IN
                 ('pending', 'running', 'succeeded', 'failed', 'aborted')),
    started_at  timestamptz,
    settled_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE runtime.run_pool_assignments (
    run_id      uuid PRIMARY KEY REFERENCES runtime.workflow_runs(id) ON DELETE CASCADE,
    pool_id     uuid NOT NULL,
    assigned_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE runtime.turn_assignments (
    turn_id                uuid PRIMARY KEY,
    run_id                 uuid NOT NULL REFERENCES runtime.workflow_runs(id) ON DELETE CASCADE,
    pool_id                uuid NOT NULL,
    worker_id              text NOT NULL,
    fencing_generation     bigint NOT NULL,
    attempt_id             text NOT NULL,
    scope_identity_id      uuid NOT NULL,
    agent_pool_key         text NOT NULL,
    model_permission       jsonb NOT NULL DEFAULT '{}'::jsonb,
    capability_request     jsonb NOT NULL DEFAULT '[]'::jsonb,
    tool_version_snapshot  jsonb NOT NULL DEFAULT '[]'::jsonb,
    state                  text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'terminal', 'fenced')),
    highest_applied_sequence bigint NOT NULL DEFAULT 0,
    assigned_at            timestamptz NOT NULL DEFAULT now(),
    terminalized_at        timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_turn_assignments_active ON runtime.turn_assignments (turn_id) WHERE state = 'active';
`
