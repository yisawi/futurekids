-- APP-COMPATIBILITY: none — the Go code always supplies parents.pin_code. It restores the plaintext DEFAULT '1234', so
--   parents seeded by SQL without a bcrypt PIN can never log in again (audit warning W15).
-- Data loss: none.

ALTER TABLE parents ALTER COLUMN pin_code SET DEFAULT '1234';
