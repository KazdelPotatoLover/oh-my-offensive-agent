-- Migration 008 Down: Revert Scope Policy Versioning
ALTER TABLE runs DROP COLUMN IF EXISTS scope_policy_version;
