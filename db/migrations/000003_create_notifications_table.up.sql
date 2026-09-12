CREATE TABLE IF NOT EXISTS notifications (
    id SERIAL PRIMARY KEY,
    parent_phone VARCHAR(20) NOT NULL,
    title VARCHAR(255) NOT NULL,
    body TEXT NOT NULL,
    is_read BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- تسريع جلب الإشعارات بناءً على رقم هاتف ولي الأمر
CREATE INDEX idx_notifications_phone ON notifications(parent_phone);