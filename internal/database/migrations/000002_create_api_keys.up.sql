CREATE TABLE api_keys (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL,
    key_hash        TEXT NOT NULL UNIQUE,
    key_prefix      TEXT NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT true,

    -- Rate limits for this key (null = use global defaults).
    rpm_limit       INT,
    tpm_limit       INT,

    -- Restrict key to specific models (null = all models allowed).
    allowed_models  TEXT[],

    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_api_keys_key_hash ON api_keys(key_hash);
CREATE INDEX idx_api_keys_active ON api_keys(active) WHERE active = true;
