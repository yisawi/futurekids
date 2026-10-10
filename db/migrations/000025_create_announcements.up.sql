-- Announcements the admin sends to parents from the dashboard: to every parent, to one parent, or
-- to the parents of a grade, a section or a class. Each recipient gets an ordinary notification
-- row that points back here through notifications.announcement_id, so the sent log can count how
-- many were read. An announcement is never edited or deleted by the application.
-- Old application code keeps running on this schema: it only adds a table and a nullable column
-- (with its index), and every existing insert into notifications leaves announcement_id NULL.

CREATE TABLE IF NOT EXISTS announcements (
    id SERIAL PRIMARY KEY,
    title TEXT NOT NULL CHECK (char_length(title) BETWEEN 1 AND 100),
    body TEXT NOT NULL CHECK (char_length(body) BETWEEN 1 AND 500),
    audience_type TEXT NOT NULL CHECK (audience_type IN ('all', 'parent', 'class')),
    audience_grade TEXT CHECK (char_length(audience_grade) BETWEEN 1 AND 50),
    audience_section TEXT CHECK (char_length(audience_section) BETWEEN 1 AND 50),
    audience_parent_id INT REFERENCES parents(id) ON DELETE SET NULL,
    recipient_count INT NOT NULL CHECK (recipient_count >= 0),
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT announcements_audience_fields CHECK (
        (audience_type = 'all' AND audience_grade IS NULL AND audience_section IS NULL AND audience_parent_id IS NULL)
        OR (audience_type = 'parent' AND audience_grade IS NULL AND audience_section IS NULL)
        OR (audience_type = 'class' AND audience_parent_id IS NULL AND (audience_grade IS NOT NULL OR audience_section IS NOT NULL))
    )
);

ALTER TABLE notifications ADD COLUMN IF NOT EXISTS announcement_id INT REFERENCES announcements(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_notifications_announcement_id ON notifications (announcement_id) WHERE announcement_id IS NOT NULL;
