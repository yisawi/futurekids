-- The school name printed on the Excel attendance report (and exposed by /api/mobile/settings)
-- was hard-coded; it is now the 'school_name' setting, editable via PUT /api/admin/settings.
INSERT INTO settings (setting_key, setting_value)
VALUES ('school_name', 'مدرسة الرحمن الابتدائية الأهلية')
ON CONFLICT (setting_key) DO NOTHING;
