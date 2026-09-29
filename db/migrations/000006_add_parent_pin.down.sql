-- APP-COMPATIBILITY: none — the current Go code does not use students.parent_pin (PINs live in parents.pin_code since 000009).
-- Data loss: students.parent_pin values (they exist only after 000009 was rolled back).

ALTER TABLE students DROP COLUMN IF EXISTS parent_pin;
