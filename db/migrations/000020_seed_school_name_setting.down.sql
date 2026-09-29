-- APP-COMPATIBILITY: none — without the row, the Excel export prints a blank school-name line (and logs a
--   warning) and /api/mobile/settings omits school_name; nothing fails.
-- Data loss: the school_name setting, including any edit made through PUT /api/admin/settings.

DELETE FROM settings WHERE setting_key = 'school_name';
