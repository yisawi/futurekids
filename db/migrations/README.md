# Database Migrations

Raw SQL migrations in [golang-migrate](https://github.com/golang-migrate/migrate) format: `NNNNNN_name.up.sql` applies a change and `NNNNNN_name.down.sql` reverts it. Files are applied in numeric order; never edit a migration that has already run anywhere — add a new one instead.

## Rolling back

Down-migrations are **destructive**. Most drop tables or columns, and the data in them is gone for good. Every `.down.sql` file starts with a header that tells you what rolling it back does:

- `-- ⚠ APP-COMPATIBILITY WARNING` — the current app code depends on what this rollback removes. The header lists what is removed, every Go file and handler that breaks, and what data is lost.
- `-- APP-COMPATIBILITY: none — <reason>` — the current app code is unaffected; the header says why and what data, if any, is lost.

Before running any down-migration against a shared database:

1. **Read its header.** If it carries the warning, the running server will start failing the moment the migration completes.
2. **Back up the database:** `pg_dump "$DATABASE_URL" > backup.sql`
3. **Deploy app code that matches the schema you are rolling back to** (revert or update the files named in the header) *before* or together with the rollback.
4. Roll back one step at a time: `migrate -path db/migrations -database "$DATABASE_URL" down 1`

Check `SELECT version, dirty FROM schema_migrations;` first — if the recorded version does not match the real schema, fix it with `migrate force <version>` before going up or down.

The headers are verified by `tests/warnings/w14_down_migration_comments_test.go`, which fails if a down-migration is added without a header or if new Go code starts using an object a rollback removes without being named in its warning.
