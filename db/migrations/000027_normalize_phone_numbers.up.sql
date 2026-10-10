-- Rewrites every stored phone number to the canonical Iraqi mobile form +9647XXXXXXXXX
-- (RULES.md §3), the form internal/phone.Normalize produces for logins and admin input.
-- The previous value is kept in *_original so the down-migration can restore it.
-- The file has no BEGIN/COMMIT: sent as one query it runs as a single implicit transaction, so
-- it fails, changing nothing and leaving the session usable, when a parent's number is not an Iraqi mobile number or when two
-- parents' numbers are the same number in different formats; the error lists the parent ids.
-- Old application code keeps running on this schema: the new columns are nullable and unused.

CREATE OR REPLACE FUNCTION pg_temp.fk_iq_mobile(raw TEXT) RETURNS TEXT LANGUAGE sql IMMUTABLE AS $$
    WITH c AS (
        SELECT translate(
            translate(raw, '٠١٢٣٤٥٦٧٨٩۰۱۲۳۴۵۶۷۸۹', '01234567890123456789'),
            ' ' || chr(9) || '-().' || chr(160) || chr(8201) || chr(8239) || chr(8206) || chr(8207) || chr(8204) || chr(8205),
            '') AS s
    ), n AS (
        SELECT CASE
            WHEN s LIKE '+964%' THEN regexp_replace(substr(s, 5), '^0', '')
            WHEN s LIKE '00964%' THEN regexp_replace(substr(s, 6), '^0', '')
            WHEN s LIKE '964%' THEN regexp_replace(substr(s, 4), '^0', '')
            WHEN s LIKE '0%' THEN substr(s, 2)
        END AS nsn
        FROM c
    )
    SELECT CASE WHEN nsn ~ '^7[0-9]{9}$' THEN '+964' || nsn END FROM n
$$;

DO $$
DECLARE
    invalid TEXT;
    duplicates TEXT;
BEGIN
    SELECT string_agg(id::TEXT, ', ' ORDER BY id) INTO invalid
    FROM parents WHERE pg_temp.fk_iq_mobile(phone_number) IS NULL;
    IF invalid IS NOT NULL THEN
        RAISE EXCEPTION 'phone normalisation aborted: parents % have a phone number that is not an Iraqi mobile number; correct them and rerun', invalid;
    END IF;

    SELECT string_agg('[' || ids || ']', ' ') INTO duplicates FROM (
        SELECT string_agg(id::TEXT, ', ' ORDER BY id) AS ids
        FROM parents
        GROUP BY pg_temp.fk_iq_mobile(phone_number)
        HAVING COUNT(*) > 1
    ) d;
    IF duplicates IS NOT NULL THEN
        RAISE EXCEPTION 'phone normalisation aborted: parents % have the same phone number in different formats; merge each group into one parent and rerun', duplicates;
    END IF;
END $$;

ALTER TABLE parents ADD COLUMN IF NOT EXISTS phone_number_original VARCHAR(20);
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS parent_phone_original VARCHAR(20);

UPDATE parents
SET phone_number_original = phone_number,
    phone_number = pg_temp.fk_iq_mobile(phone_number)
WHERE phone_number <> pg_temp.fk_iq_mobile(phone_number);

UPDATE notifications
SET parent_phone_original = parent_phone,
    parent_phone = pg_temp.fk_iq_mobile(parent_phone)
WHERE pg_temp.fk_iq_mobile(parent_phone) IS NOT NULL
  AND parent_phone <> pg_temp.fk_iq_mobile(parent_phone);
