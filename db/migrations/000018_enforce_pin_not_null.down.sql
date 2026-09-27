-- Restores the 000009 state: pin_code stays NOT NULL (as 000009 created it) and regains the
-- plaintext DEFAULT '1234'. The Go code always supplies pin_code, so it is unaffected.
ALTER TABLE parents ALTER COLUMN pin_code SET DEFAULT '1234';
