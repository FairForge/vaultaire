-- Email verification columns on users table.
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified BOOLEAN DEFAULT FALSE;
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verify_token VARCHAR(255);
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verify_sent_at TIMESTAMP;

CREATE INDEX IF NOT EXISTS idx_users_verify_token ON users(email_verify_token) WHERE email_verify_token IS NOT NULL;
