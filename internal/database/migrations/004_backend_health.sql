-- Backend health tracking
CREATE TABLE IF NOT EXISTS backend_health (
    id SERIAL PRIMARY KEY,
    backend_id VARCHAR(255) NOT NULL,
    timestamp TIMESTAMP NOT NULL DEFAULT NOW(),
    score FLOAT NOT NULL,
    latency_ms INTEGER NOT NULL,
    error_rate FLOAT NOT NULL,
    uptime FLOAT NOT NULL,
    throughput_bps BIGINT NOT NULL,
    last_error TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_backend_health_backend_time ON backend_health(backend_id, timestamp DESC);
CREATE INDEX idx_backend_health_score ON backend_health(score);
