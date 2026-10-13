-- Notifications belong to parent_id (000023); the code from this migration on stores no phone
-- number with them. Old application code keeps running on this schema: it still writes
-- parent_phone. 000031 drops the column once the new code is deployed and checked.

ALTER TABLE notifications ALTER COLUMN parent_phone DROP NOT NULL;
