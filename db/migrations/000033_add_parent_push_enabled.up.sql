-- push_enabled is the parent's "receive push notifications on my phone" switch. While it is false
-- the parent has no device tokens (turning it off deletes them and registration stores none), so
-- no push is ever sent; notifications are still written to the parent's in-app list.
-- Old application code keeps running on this schema: it never reads the column, and every parent
-- it creates gets the default.
ALTER TABLE parents ADD COLUMN IF NOT EXISTS push_enabled BOOLEAN NOT NULL DEFAULT TRUE;
