-- APP-COMPATIBILITY: none — the current Go code does not use students.parent_phone (phones live in parents.phone_number since 000009).
-- Data loss: students.parent_phone values.

ALTER TABLE students DROP COLUMN IF EXISTS parent_phone;
