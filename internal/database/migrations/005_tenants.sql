CREATE TABLE IF NOT EXISTS tenants (
    id          VARCHAR(255) PRIMARY KEY,
    name        VARCHAR(255) NOT NULL,
    email       VARCHAR(255) UNIQUE NOT NULL,
    access_key  VARCHAR(255) UNIQUE,
    secret_key  VARCHAR(255),
    created_at  TIMESTAMP DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_tenants_access_key ON tenants(access_key);
CREATE INDEX IF NOT EXISTS idx_tenants_email ON tenants(email);
