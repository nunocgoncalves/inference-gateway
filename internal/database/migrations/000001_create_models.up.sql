CREATE TABLE models (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL UNIQUE,
    model_id        TEXT NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT true,

    -- Default sampling parameters (overridable per request).
    -- Example: {"temperature": 0.7, "top_p": 0.9, "max_tokens": 4096}
    default_params  JSONB NOT NULL DEFAULT '{}',

    -- Reasoning configuration.
    -- enabled: true  = force reasoning on  (inject reasoning_effort + include_reasoning)
    -- enabled: false = force reasoning off (inject reasoning_effort:"none", include_reasoning:false)
    -- enabled: null / absent = passthrough (no override)
    -- Example: {"enabled": true, "reasoning_effort": "medium", "include_reasoning": true}
    reasoning_config JSONB NOT NULL DEFAULT '{}',

    -- Request transform rules.
    -- Example: {"system_prompt_prefix": "You are a helpful assistant.", "rewrite_model_name": true}
    transforms      JSONB NOT NULL DEFAULT '{}',

    -- Per-model rate limits (override key-level limits when set).
    -- Example: {"rpm": 100, "tpm": 50000}
    rate_limits     JSONB NOT NULL DEFAULT '{}',

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE model_backends (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id          UUID NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    url               TEXT NOT NULL,
    weight            INT NOT NULL DEFAULT 1 CHECK (weight > 0),
    active            BOOLEAN NOT NULL DEFAULT true,

    -- Health check state (managed by gateway, not user-set).
    healthy           BOOLEAN NOT NULL DEFAULT true,
    last_health_check TIMESTAMPTZ,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE(model_id, url)
);

CREATE INDEX idx_model_backends_model_id ON model_backends(model_id);
CREATE INDEX idx_models_active ON models(active) WHERE active = true;
