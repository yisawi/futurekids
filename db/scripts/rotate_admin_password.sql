-- MANUAL EXECUTION ONLY - Before production deployment
-- Replace 'NEW_PASSWORD_HERE' with the actual strong password
-- Run this once after all migrations complete, before Staging/Production goes live
--
-- Work on a gitignored copy (rotate_admin_password.with_password.sql), never on this template.
-- See db/scripts/README.md for the full procedure.

BEGIN;

-- crypt()/gen_salt() are provided by pgcrypto (a Postgres contrib extension, not new infrastructure).
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Guard: abort if the placeholder was not replaced. The aborted transaction also prevents the UPDATE below.
DO $$
BEGIN
    IF 'NEW_PASSWORD_HERE' = 'NEW' || '_PASSWORD_HERE' THEN
        RAISE EXCEPTION 'Replace NEW_PASSWORD_HERE with the real password before running this script';
    END IF;
END $$;

-- Cost 10 matches golang.org/x/crypto/bcrypt DefaultCost used by the Go code.
-- Raising session_version signs out every admin token issued before the rotation.
UPDATE admins
SET password_hash = crypt('NEW_PASSWORD_HERE', gen_salt('bf', 10)),
    session_version = session_version + 1
WHERE username = 'admin';

COMMIT;
