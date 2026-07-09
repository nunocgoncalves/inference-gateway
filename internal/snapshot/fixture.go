package snapshot

// FixtureSchema is a test-only mirror of the control-plane contract (the 4 views
// the gateway reads), NOT the control-plane's actual migrations — so gateway
// tests stay decoupled from the control-plane repo (B+C approach). Exported so
// the server integration test can reuse it.
const FixtureSchema = `
CREATE SCHEMA IF NOT EXISTS identity;
CREATE SCHEMA IF NOT EXISTS permissions;
CREATE SCHEMA IF NOT EXISTS catalog;

CREATE TABLE identity.identities (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key text NOT NULL UNIQUE,
    kind text NOT NULL,
    deleted_at timestamptz
);
CREATE TABLE identity.api_keys (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    identity_id uuid NOT NULL REFERENCES identity.identities(id) ON DELETE CASCADE,
    key_hash text NOT NULL UNIQUE,
    scope text NOT NULL,
    expires_at timestamptz,
    revoked_at timestamptz
);
CREATE TABLE permissions.policies (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_kind text NOT NULL,
    subject_key text NOT NULL,
    rate_limits jsonb,
    deleted_at timestamptz
);
CREATE VIEW permissions.effective_capabilities AS
    SELECT i.id AS identity_id, '*'::text AS resource, '*'::text AS action
    FROM identity.identities i WHERE i.deleted_at IS NULL;
CREATE VIEW permissions.effective_rate_limits AS
    SELECT i.id AS identity_id,
           MIN((p.rate_limits->>'rpm')::int) AS rpm,
           MIN((p.rate_limits->>'tpm')::int) AS tpm
    FROM permissions.policies p
    JOIN identity.identities i ON i.key = p.subject_key
    WHERE p.deleted_at IS NULL AND i.deleted_at IS NULL AND p.rate_limits IS NOT NULL
    GROUP BY i.id;
CREATE TABLE catalog.backends (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key text NOT NULL UNIQUE,
    name text NOT NULL,
    namespace text NOT NULL,
    kind text NOT NULL,
    model text,
    service_url text NOT NULL,
    deployed boolean NOT NULL DEFAULT false,
    healthy boolean NOT NULL DEFAULT false,
    deleted_at timestamptz
);
CREATE TABLE catalog.models (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key text NOT NULL UNIQUE,
    namespace text NOT NULL,
    model_id text NOT NULL,
    display_name text,
    context_length integer,
    capabilities jsonb NOT NULL DEFAULT '[]'::jsonb,
    backend_ref text NOT NULL,
    default_params jsonb NOT NULL DEFAULT '{}'::jsonb,
    reasoning_config jsonb NOT NULL DEFAULT '{}'::jsonb,
    transforms jsonb NOT NULL DEFAULT '{}'::jsonb,
    rate_limits jsonb NOT NULL DEFAULT '{}'::jsonb,
    available boolean NOT NULL DEFAULT false,
    deleted_at timestamptz
);
CREATE VIEW catalog.effective_catalog AS
    SELECT m.model_id, m.display_name, m.context_length, m.capabilities, m.backend_ref,
           b.kind AS backend_kind, b.model AS backend_model_id, b.service_url AS backend_url,
           m.default_params, m.reasoning_config, m.transforms, m.rate_limits,
           (m.available AND b.healthy) AS available
    FROM catalog.models m
    JOIN catalog.backends b ON b.name = m.backend_ref AND b.namespace = m.namespace AND b.deleted_at IS NULL
    WHERE m.deleted_at IS NULL;
`
