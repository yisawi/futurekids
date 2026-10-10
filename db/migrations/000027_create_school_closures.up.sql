-- School closures the admin registers from the dashboard: an official holiday (one day, or a date
-- range of at most 60 days) or a pause (a long closure such as the summer break, open-ended until
-- it is cancelled). A closed date is not a school day: no absences are counted, the noon absence
-- job does nothing and punches are stored without notifications. Friday and Saturday are never
-- school days and need no row here. broadcast_id links the notification sent to parents when the
-- closure was registered.
-- Old application code keeps running on this schema: it only adds a table, which nothing reads.

CREATE TABLE IF NOT EXISTS school_closures (
    id SERIAL PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('holiday', 'pause')),
    start_date DATE NOT NULL,
    end_date DATE,
    title TEXT NOT NULL CHECK (char_length(title) BETWEEN 1 AND 80),
    notes TEXT CHECK (char_length(notes) <= 500),
    broadcast_id INT REFERENCES broadcasts(id) ON DELETE SET NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT school_closures_dates CHECK (end_date IS NULL OR end_date >= start_date),
    CONSTRAINT school_closures_holiday_length CHECK (kind <> 'holiday' OR (end_date IS NOT NULL AND end_date - start_date < 60))
);

CREATE INDEX IF NOT EXISTS idx_school_closures_dates ON school_closures (start_date, end_date);
