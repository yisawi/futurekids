BEGIN;

CREATE TABLE settings (
    setting_key VARCHAR(100) PRIMARY KEY,
    setting_value TEXT NOT NULL,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- إدراج القيمة الافتراضية
INSERT INTO settings (setting_key, setting_value) 
VALUES ('whatsapp_number', '+9640000000000');

COMMIT;