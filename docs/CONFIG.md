# Configuration Guide

## Configuration Methods

Vaultaire can be configured through:
1. YAML configuration file
2. Environment variables
3. Command-line flags
4. API (for dynamic settings)

Priority: CLI flags > Environment > Config file > Defaults

## Configuration File

Default location: `/etc/vaultaire/config.yaml`

### Basic Configuration

```yaml
# config.yaml
server:
  port: 9000
  host: 0.0.0.0
  tls:
    enabled: false
    cert: /path/to/cert.pem
    key: /path/to/key.pem

engine:
  tier_policy: automatic
  cache_size: 1GB

backends:
  - name: primary
    driver: s3
    config:
      endpoint: s3.amazonaws.com
      region: us-east-1
      bucket: my-storage
      access_key: ${AWS_ACCESS_KEY}
      secret_key: ${AWS_SECRET_KEY}

  - name: backup
    driver: wasabi
    config:
      endpoint: s3.wasabisys.com
      bucket: backup-storage
      access_key: ${WASABI_ACCESS_KEY}
      secret_key: ${WASABI_SECRET_KEY}

database:
  driver: postgres
  connection: postgres://user:pass@localhost/vaultaire

logging:
  level: info
  format: json
  output: stdout
Advanced Configuration
yaml# Advanced tiering configuration
tiering:
  policies:
    - name: hot
      age: 0-7d
      backend: primary

    - name: warm
      age: 7-30d
      backend: secondary

    - name: cold
      age: 30d+
      backend: archive

# Replication settings
replication:
  enabled: true
  factor: 3
  strategy: async
  backends:
    - primary
    - backup
    - archive

# Performance tuning
performance:
  max_connections: 1000
  worker_threads: 16
  buffer_size: 64MB

# Monitoring
monitoring:
  prometheus:
    enabled: true
    port: 9090

  metrics:
    enabled: true
    interval: 30s
Environment Variables
All configuration options can be set via environment variables:
bash# Server settings
VAULTAIRE_SERVER_PORT=9000
VAULTAIRE_SERVER_HOST=0.0.0.0

# Backend configuration
VAULTAIRE_BACKEND_PRIMARY_DRIVER=s3
VAULTAIRE_BACKEND_PRIMARY_ENDPOINT=s3.amazonaws.com
VAULTAIRE_BACKEND_PRIMARY_ACCESS_KEY=xxx
VAULTAIRE_BACKEND_PRIMARY_SECRET_KEY=yyy

# Database
VAULTAIRE_DATABASE_CONNECTION=postgres://localhost/vaultaire

# Logging
VAULTAIRE_LOG_LEVEL=debug
VAULTAIRE_LOG_FORMAT=json
Command-Line Flags
bashvaultaire serve \
  --port 9000 \
  --config /etc/vaultaire/config.yaml \
  --log-level debug \
  --backend s3://bucket \
  --cache-size 2GB
Backend-Specific Configuration
S3 / S3-Compatible
yamldriver: s3
config:
  endpoint: s3.amazonaws.com
  region: us-east-1
  bucket: my-bucket
  access_key: xxx
  secret_key: yyy
  use_ssl: true
  path_style: false
Seagate Lyve Cloud
yamldriver: lyve
config:
  endpoint: s3.lyvecloud.seagate.com
  region: us-east-1
  bucket: my-bucket
  access_key: xxx
  secret_key: yyy
Local Filesystem
yamldriver: local
config:
  path: /var/lib/vaultaire/storage
  permissions: 0755
Wasabi
yamldriver: wasabi
config:
  endpoint: s3.wasabisys.com
  region: us-east-1
  bucket: my-bucket
  access_key: xxx
  secret_key: yyy
Security Configuration
API Authentication
yamlauth:
  enabled: true
  type: api_key

  # For API key auth
  api_key:
    header: X-API-Key

  # For JWT auth
  jwt:
    secret: ${JWT_SECRET}
    expiry: 24h
Encryption
yamlencryption:
  at_rest:
    enabled: true
    type: aes256
    key: ${ENCRYPTION_KEY}

  in_transit:
    enabled: true
    min_tls_version: "1.2"
Performance Tuning
Cache Settings
yamlcache:
  metadata:
    size: 1GB
    ttl: 1h

  data:
    size: 10GB
    strategy: lru

  distributed:
    enabled: true
    redis: redis://localhost:6379
Connection Pooling
yamlconnections:
  max_idle: 10
  max_open: 100
  max_lifetime: 1h
  timeout: 30s
Monitoring Configuration
Prometheus Metrics
yamlmetrics:
  prometheus:
    enabled: true
    port: 9090
    path: /metrics

  custom:
    - name: storage_operations_total
      type: counter
      labels: [operation, backend]

    - name: storage_latency_seconds
      type: histogram
      labels: [operation]
Health Checks
yamlhealth:
  endpoint: /health
  checks:
    - name: database
      interval: 30s
      timeout: 5s

    - name: backends
      interval: 60s
      timeout: 10s
Migration from Other Systems
From MinIO
bash# Export from MinIO
mc mirror minio/bucket /tmp/export

# Import to Vaultaire
vaultaire import --from /tmp/export --to container
From S3
bash# Use AWS CLI
aws s3 sync s3://old-bucket s3://new-bucket \
  --endpoint-url https://api.stored.ge
Troubleshooting
Debug Mode
bashVAULTAIRE_LOG_LEVEL=debug vaultaire serve
Verbose Output
bashvaultaire serve -v -v -v
Configuration Validation
bashvaultaire config validate --file config.yaml
