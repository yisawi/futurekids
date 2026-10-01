-- session_version rises whenever a parent's PIN or the admin password changes. Tokens carry
-- the value they were issued with (claim "sv") and are rejected once it no longer matches.
-- Old application code keeps running on this schema: both columns default to 0 and are unused.
ALTER TABLE parents ADD COLUMN IF NOT EXISTS session_version INT NOT NULL DEFAULT 0;
ALTER TABLE admins ADD COLUMN IF NOT EXISTS session_version INT NOT NULL DEFAULT 0;
