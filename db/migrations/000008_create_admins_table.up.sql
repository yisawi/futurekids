CREATE TABLE IF NOT EXISTS admins (
    id SERIAL PRIMARY KEY,
    username VARCHAR(50) UNIQUE NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    created_at TIMESTAMP DEFAULT NOW()
);

-- This bcrypt hash is for the initial password: admin123
INSERT INTO admins (username, password_hash) 
VALUES ('admin', '$2a$10$rqj1EYSmatHsczichRrom.iucB.qnMTgPrqNGC9POTQHunNxZ4hNW')
ON CONFLICT DO NOTHING;