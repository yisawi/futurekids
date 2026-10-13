-- What was done in the admin dashboard: one row per admin write request (and per Excel export
-- and admin login attempt), written after the handler finished, with the real status. Only
-- identifiers and counts are stored: never secrets, names, phone numbers or free text.
-- Deliberately not tamper-proof: no triggers or rules, so the owner can correct or delete rows
-- by SQL. The application never updates or deletes a row.
-- Old application code keeps running on this schema: it only adds a table, which nothing reads.

CREATE TABLE IF NOT EXISTS admin_activity_log (
    id BIGSERIAL PRIMARY KEY,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    actor TEXT NOT NULL CHECK (char_length(actor) BETWEEN 1 AND 50),
    action TEXT NOT NULL CHECK (char_length(action) BETWEEN 1 AND 60),
    method TEXT NOT NULL,
    path TEXT NOT NULL CHECK (char_length(path) <= 200),
    status SMALLINT NOT NULL,
    ip TEXT CHECK (char_length(ip) <= 64),
    target_type TEXT CHECK (char_length(target_type) <= 40),
    target_id TEXT CHECK (char_length(target_id) <= 64),
    detail TEXT CHECK (char_length(detail) <= 200)
);
