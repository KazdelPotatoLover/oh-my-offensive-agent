-- Migration 008: Scope Policy Versioning and HTTP Assets Support
ALTER TABLE runs ADD COLUMN IF NOT EXISTS scope_policy_version VARCHAR(32) NOT NULL DEFAULT 'v1';

-- Existing historical runs prior to migration 008 are tagged as legacy
UPDATE runs SET scope_policy_version = 'legacy' WHERE created_at < now();
