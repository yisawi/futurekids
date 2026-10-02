-- Push notification tokens belong to a parent's phones, not to students: every device a parent
-- registered receives the pushes for all of that parent's children (RULES.md §4). A token is
-- unique across the table because a phone belongs to one parent at a time.
-- Backfills from students.fcm_token, which old code still reads and writes and which this
-- migration leaves untouched, so the old code runs unchanged on the new schema. The backfill
-- copies each well-formed token once, to the parent of the newest student holding it, at most
-- 10 per parent (internal/handlers.MaxDeviceTokensPerParent); re-running it adds nothing.

CREATE TABLE IF NOT EXISTS device_tokens (
    id BIGSERIAL PRIMARY KEY,
    parent_id INT NOT NULL REFERENCES parents(id) ON DELETE CASCADE,
    token TEXT NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_device_tokens_parent_id ON device_tokens (parent_id);

INSERT INTO device_tokens (parent_id, token)
SELECT parent_id, fcm_token
FROM (
    SELECT parent_id, fcm_token,
           ROW_NUMBER() OVER (PARTITION BY parent_id ORDER BY newest_student DESC) AS rank
    FROM (
        SELECT DISTINCT ON (fcm_token) fcm_token, parent_id, id AS newest_student
        FROM students
        WHERE parent_id IS NOT NULL
          AND fcm_token ~ '^[A-Za-z0-9:_.-]+$'
          AND char_length(fcm_token) BETWEEN 20 AND 1024
        ORDER BY fcm_token, id DESC
    ) latest
) ranked
WHERE rank <= 10
ON CONFLICT (token) DO NOTHING;
