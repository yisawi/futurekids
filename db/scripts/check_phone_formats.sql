-- READ-ONLY check to run on a database BEFORE migration 000024 (phone normalisation).
-- It changes nothing (READ ONLY transaction, rolled back) and prints no phone numbers: only
-- number shapes (digits shown as 9), counts, and parent ids. Its normalisation is the same as
-- 000024 and internal/phone.Normalize (checked by tests/warnings/a1_a4_security_sessions_test.go).
--   invalid parent ids            — numbers 000024 cannot convert; it would abort on them.
--   collision groups (parent ids) — parents whose numbers are one number; 000024 would abort.
--   would change / already canonical — rows 000024 rewrites or leaves alone.
-- Usage: psql "$DATABASE_URL" -X -f db/scripts/check_phone_formats.sql

BEGIN READ ONLY;

WITH p AS (
    SELECT id, phone_number,
           translate(translate(phone_number, '٠١٢٣٤٥٦٧٨٩۰۱۲۳۴۵۶۷۸۹', '01234567890123456789'),
                     ' ' || chr(9) || '-().' || chr(160) || chr(8201) || chr(8239) || chr(8206) || chr(8207) || chr(8204) || chr(8205), '') AS s
    FROM parents
), n AS (
    SELECT id, phone_number,
           CASE
               WHEN s LIKE '+964%' THEN regexp_replace(substr(s, 5), '^0', '')
               WHEN s LIKE '00964%' THEN regexp_replace(substr(s, 6), '^0', '')
               WHEN s LIKE '964%' THEN regexp_replace(substr(s, 4), '^0', '')
               WHEN s LIKE '0%' THEN substr(s, 2)
           END AS nsn
    FROM p
), c AS (
    SELECT id, phone_number, CASE WHEN nsn ~ '^7[0-9]{9}$' THEN '+964' || nsn END AS canonical FROM n
)
SELECT 'shape' AS kind, regexp_replace(phone_number, '[0-9٠-٩۰-۹]', '9', 'g') AS detail, COUNT(*)::text AS value
FROM c GROUP BY 2
UNION ALL
SELECT 'already canonical', '', COUNT(*)::text FROM c WHERE phone_number = canonical
UNION ALL
SELECT 'would change', '', COUNT(*)::text FROM c WHERE canonical IS NOT NULL AND phone_number <> canonical
UNION ALL
SELECT 'invalid parent ids', '', COALESCE(string_agg(id::text, ', ' ORDER BY id), 'none') FROM c WHERE canonical IS NULL
UNION ALL
SELECT 'collision groups (parent ids)', '', COALESCE(string_agg(ids, ' '), 'none')
FROM (SELECT '[' || string_agg(id::text, ', ' ORDER BY id) || ']' AS ids FROM c WHERE canonical IS NOT NULL GROUP BY canonical HAVING COUNT(*) > 1) g
UNION ALL
SELECT 'notification shape', regexp_replace(parent_phone, '[0-9٠-٩۰-۹]', '9', 'g'), COUNT(*)::text FROM notifications GROUP BY 2
ORDER BY 1, 2;

ROLLBACK;
