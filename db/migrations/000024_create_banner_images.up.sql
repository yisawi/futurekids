-- Banner pictures uploaded from the admin dashboard, one per banner, stored in the database
-- (a school has a few dozen banners a year). They live in their own table so that listing
-- banners never reads image bytes. The content type is detected from the bytes by the server;
-- checksum is the SHA-256 of data in hex and serves as the HTTP ETag.
-- Old application code keeps running on this schema: it only adds a table, and banners is
-- unchanged (banners.image_url stays NOT NULL; banners with an uploaded picture store '').
-- Pictures are already compressed, so data is stored without TOAST compression.

CREATE TABLE IF NOT EXISTS banner_images (
    banner_id INT PRIMARY KEY REFERENCES banners(id) ON DELETE CASCADE,
    content_type TEXT NOT NULL CHECK (content_type IN ('image/jpeg', 'image/png', 'image/webp')),
    size_bytes INT NOT NULL CHECK (size_bytes BETWEEN 1 AND 2097152),
    checksum TEXT NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
    data BYTEA NOT NULL CHECK (octet_length(data) = size_bytes),
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE banner_images ALTER COLUMN data SET STORAGE EXTERNAL;
