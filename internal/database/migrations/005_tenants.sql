CREATE TABLE IF NOT EXISTS tenants (
    id          VARCHAR(255) PRIMARY KEY,
    name        VARCHAR(255) NOT NULL,
    email       VARCHAR(255) UNIQUE NOT NULL,
    access_key  VARCHAR(255) UNIQUE,
    secret_key  VARCHAR(255),
    created_at  TIMESTAMP DEFAULT NOW()
);
-- idx_tenants_access_key and idx_tenants_email duplicated the UNIQUE indexes
-- of the same columns: dropped by 119_index_hygiene.sql, their CREATE removed
-- here in the same change (the deploy re-applies every file).
