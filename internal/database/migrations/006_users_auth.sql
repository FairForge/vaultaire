CREATE TABLE IF NOT EXISTS users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email VARCHAR(255) UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    company VARCHAR(255),
    stripe_customer_id VARCHAR(255),
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    key_id VARCHAR(255) UNIQUE NOT NULL,
    secret_hash TEXT NOT NULL,
    last_used TIMESTAMP,
    created_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- idx_users_email and idx_api_keys_key_id duplicated the UNIQUE indexes of
-- users.email and api_keys.key_id: dropped by 119_index_hygiene.sql, their
-- CREATE removed here in the same change (the deploy re-applies every file).
CREATE INDEX IF NOT EXISTS idx_api_keys_user_id ON api_keys(user_id);
