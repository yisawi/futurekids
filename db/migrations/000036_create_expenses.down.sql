-- ⚠ APP-COMPATIBILITY WARNING
-- Removes: tables expenses and expense_categories.
-- Running this down-migration against the current main branch will break:
--   internal/handlers/expenses.go — every /api/admin/expenses route answers 500.
-- Data loss: every expense (voided ones included) and every category the admin added.
-- Before running it: back up the database (pg_dump "$DATABASE_URL" > backup.sql) and redeploy
-- app code that no longer uses these objects. Do not run this migrate-down without first
-- reverting or updating those files to match the pre-migration schema.

DROP TABLE IF EXISTS expenses;
DROP TABLE IF EXISTS expense_categories;
