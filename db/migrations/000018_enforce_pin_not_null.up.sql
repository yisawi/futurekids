-- pin_code must always be supplied explicitly as a bcrypt hash (MobileLoginHandler compares with bcrypt).
-- 000009 created it NOT NULL but with DEFAULT '1234' in plaintext, so a parent seeded via SQL
-- without a PIN silently got a value that can never log in. Dropping the default makes such
-- inserts fail loudly instead. Existing rows are not backfilled.
ALTER TABLE parents ALTER COLUMN pin_code SET NOT NULL;
ALTER TABLE parents ALTER COLUMN pin_code DROP DEFAULT;
