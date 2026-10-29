-- Tuition, in whole Iraqi dinars, per academic year (the 'academic_year' setting, YYYY-YYYY).
-- tuition_fees is the admin's fee per grade and year. tuition_accounts freezes one student's grade,
-- fee and discount for one year: it is created on the student's first discount or payment, and for
-- every active student whose grade has a fee when the academic year setting changes, so a later fee
-- edit, grade change or year change never rewrites an existing balance. tuition_payments are never
-- edited or deleted: a wrong one is voided (voided_at and void_reason) and excluded from every sum.
-- The remaining balance (fee - discount - non-voided payments) is computed on read, never stored.
-- Old application code keeps running on this schema: it only adds tables, which nothing old reads,
-- and a setting, which the old settings endpoints list and store like any other.

CREATE TABLE IF NOT EXISTS tuition_fees (
    id SERIAL PRIMARY KEY,
    academic_year VARCHAR(9) NOT NULL CHECK (academic_year ~ '^20[0-9]{2}-20[0-9]{2}$'
        AND split_part(academic_year, '-', 2)::int = split_part(academic_year, '-', 1)::int + 1),
    grade VARCHAR(50) NOT NULL CHECK (char_length(grade) BETWEEN 1 AND 50),
    fee BIGINT NOT NULL CHECK (fee BETWEEN 1 AND 1000000000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT tuition_fees_year_grade_key UNIQUE (academic_year, grade)
);

CREATE TABLE IF NOT EXISTS tuition_accounts (
    id SERIAL PRIMARY KEY,
    student_id INT NOT NULL REFERENCES students(id),
    academic_year VARCHAR(9) NOT NULL CHECK (academic_year ~ '^20[0-9]{2}-20[0-9]{2}$'
        AND split_part(academic_year, '-', 2)::int = split_part(academic_year, '-', 1)::int + 1),
    grade VARCHAR(50) NOT NULL CHECK (char_length(grade) BETWEEN 1 AND 50),
    fee BIGINT NOT NULL CHECK (fee BETWEEN 1 AND 1000000000),
    discount BIGINT NOT NULL DEFAULT 0 CHECK (discount >= 0 AND discount <= fee),
    discount_note TEXT CHECK (char_length(discount_note) BETWEEN 1 AND 200),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT tuition_accounts_student_year_key UNIQUE (student_id, academic_year)
);

CREATE TABLE IF NOT EXISTS tuition_payments (
    id BIGSERIAL PRIMARY KEY,
    account_id INT NOT NULL REFERENCES tuition_accounts(id),
    amount BIGINT NOT NULL CHECK (amount BETWEEN 1 AND 1000000000),
    paid_at DATE NOT NULL,
    note TEXT CHECK (char_length(note) BETWEEN 1 AND 200),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    voided_at TIMESTAMPTZ,
    void_reason TEXT CHECK (char_length(void_reason) BETWEEN 1 AND 200),
    CONSTRAINT tuition_payments_void_check CHECK ((voided_at IS NULL) = (void_reason IS NULL))
);

CREATE INDEX IF NOT EXISTS idx_tuition_payments_account ON tuition_payments (account_id);

INSERT INTO settings (setting_key, setting_value)
VALUES ('academic_year', '2026-2027')
ON CONFLICT (setting_key) DO NOTHING;
