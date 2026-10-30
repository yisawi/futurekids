-- Expenses: what the school actually paid, in whole Iraqi dinars, per academic year (the
-- 'academic_year' setting). expense_categories are added by the admin (never renamed or deleted);
-- exactly one, the seeded رواتب, is the salary category (is_salary), and only its expenses may name
-- a teacher (checked by the application: a category's is_salary never changes). Expenses are never
-- edited or deleted: a wrong one is voided (voided_at and void_reason) and excluded from every sum.
-- Old application code keeps running on this schema: it only adds tables, which nothing old reads.

CREATE TABLE IF NOT EXISTS expense_categories (
    id SERIAL PRIMARY KEY,
    name VARCHAR(50) NOT NULL CHECK (char_length(name) BETWEEN 1 AND 50 AND name = btrim(name)),
    is_salary BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS expense_categories_name_key ON expense_categories (lower(name));
CREATE UNIQUE INDEX IF NOT EXISTS expense_categories_one_salary ON expense_categories (is_salary) WHERE is_salary;

INSERT INTO expense_categories (name, is_salary) VALUES
    ('رواتب', true),
    ('إيجار', false),
    ('مولد كهرباء', false),
    ('تواصل اجتماعي', false),
    ('كافتريا', false),
    ('إنترنت', false),
    ('أخرى', false)
ON CONFLICT (lower(name)) DO NOTHING;

CREATE TABLE IF NOT EXISTS expenses (
    id BIGSERIAL PRIMARY KEY,
    category_id INT NOT NULL REFERENCES expense_categories(id),
    academic_year VARCHAR(9) NOT NULL CHECK (academic_year ~ '^20[0-9]{2}-20[0-9]{2}$'
        AND split_part(academic_year, '-', 2)::int = split_part(academic_year, '-', 1)::int + 1),
    amount BIGINT NOT NULL CHECK (amount BETWEEN 1 AND 1000000000),
    expense_date DATE NOT NULL,
    note TEXT CHECK (char_length(note) BETWEEN 1 AND 200),
    teacher_id INT REFERENCES teachers(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    voided_at TIMESTAMPTZ,
    void_reason TEXT CHECK (char_length(void_reason) BETWEEN 1 AND 200),
    CONSTRAINT expenses_void_check CHECK ((voided_at IS NULL) = (void_reason IS NULL))
);

CREATE INDEX IF NOT EXISTS idx_expenses_year_date ON expenses (academic_year, expense_date);
