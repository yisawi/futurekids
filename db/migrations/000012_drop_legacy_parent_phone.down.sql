-- APP-COMPATIBILITY: none — it only re-adds an empty students.parent_phone column that the current Go code does not use.
-- Data loss: none.

ALTER TABLE students ADD COLUMN parent_phone VARCHAR(20);
