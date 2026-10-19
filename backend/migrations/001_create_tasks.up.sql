CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE tasks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(255) NOT NULL,
    objective TEXT NOT NULL DEFAULT '',
    scope JSONB NOT NULL DEFAULT '{}',
    config JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
