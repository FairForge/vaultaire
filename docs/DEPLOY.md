# Deployment Guide

## Deployment Options

### Docker (Recommended)

#### Quick Start
```bash
docker run -d \
  --name vaultaire \
  -p 9000:9000 \
  -v /etc/vaultaire:/config \
  -v /var/lib/vaultaire:/data \
  vaultaire/core:latest
Docker Compose
yaml# docker-compose.yaml
version: '3.8'

services:
  vaultaire:
    image: vaultaire/core:latest
    ports:
      - "9000:9000"
    volumes:
      - ./config:/config
      - vaultaire-data:/data
    environment:
      - VAULTAIRE_LOG_LEVEL=info
      - VAULTAIRE_DATABASE_CONNECTION=postgres://db/vaultaire
    depends_on:
      - postgres

  postgres:
    image: postgres:15
    environment:
      - POSTGRES_DB=vaultaire
      - POSTGRES_USER=vaultaire
      - POSTGRES_PASSWORD=secret
    volumes:
      - postgres-data:/var/lib/postgresql/data

volumes:
  vaultaire-data:
  postgres-data:
Kubernetes
Helm Chart
bashhelm repo add vaultaire https://charts.vaultaire.io
helm install my-vaultaire vaultaire/vaultaire \
  --set backend.s3.enabled=true \
  --set backend.s3.bucket=my-bucket
Manual Deployment
yaml# deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vaultaire
spec:
  replicas: 3
  selector:
    matchLabels:
      app: vaultaire
  template:
    metadata:
      labels:
        app: vaultaire
    spec:
      containers:
      - name: vaultaire
        image: vaultaire/core:latest
        ports:
        - containerPort: 9000
        env:
        - name: VAULTAIRE_CONFIG
          value: /config/config.yaml
        volumeMounts:
        - name: config
          mountPath: /config
      volumes:
      - name: config
        configMap:
          name: vaultaire-config
---
apiVersion: v1
kind: Service
metadata:
  name: vaultaire
spec:
  selector:
    app: vaultaire
  ports:
  - port: 9000
    targetPort: 9000
  type: LoadBalancer
Systemd (Bare Metal)
Installation
bash# Download binary
curl -L https://github.com/fairforge/vaultaire/releases/latest/download/vaultaire-linux-amd64 \
  -o /usr/local/bin/vaultaire
chmod +x /usr/local/bin/vaultaire

# Create user
useradd -r -s /bin/false vaultaire

# Create directories
mkdir -p /etc/vaultaire /var/lib/vaultaire
chown vaultaire:vaultaire /var/lib/vaultaire
Service File
ini# /etc/systemd/system/vaultaire.service
[Unit]
Description=Vaultaire Storage Engine
After=network.target

[Service]
Type=simple
User=vaultaire
Group=vaultaire
ExecStart=/usr/local/bin/vaultaire serve --config /etc/vaultaire/config.yaml
Restart=always
RestartSec=5
StandardOutput=syslog
StandardError=syslog
SyslogIdentifier=vaultaire

[Install]
WantedBy=multi-user.target
Start Service
bashsystemctl daemon-reload
systemctl enable vaultaire
systemctl start vaultaire
systemctl status vaultaire
Production Configuration
High Availability Setup
yaml# Hub server (stateful)
hub:
  server: nyc-hub-01
  role: master
  components:
    - postgresql (primary)
    - redis (cache)
    - orchestrator

# Spoke servers (stateless)
spokes:
  - server: ams-spoke-01
    region: eu-west
    role: worker

  - server: sgp-spoke-01
    region: ap-southeast
    role: worker

  - server: sfo-spoke-01
    region: us-west
    role: worker
Load Balancing
HAProxy Configuration
global
    maxconn 4096

defaults
    mode http
    timeout connect 5000ms
    timeout client 50000ms
    timeout server 50000ms

frontend vaultaire_frontend
    bind *:443 ssl crt /etc/ssl/vaultaire.pem
    default_backend vaultaire_backend

backend vaultaire_backend
    balance roundrobin
    option httpchk GET /health
    server spoke1 spoke1.vaultaire.io:9000 check
    server spoke2 spoke2.vaultaire.io:9000 check
    server spoke3 spoke3.vaultaire.io:9000 check
Database Setup
PostgreSQL
sql-- Create database
CREATE DATABASE vaultaire;
CREATE USER vaultaire WITH ENCRYPTED PASSWORD 'secure-password';
GRANT ALL PRIVILEGES ON DATABASE vaultaire TO vaultaire;

-- Create schema
\c vaultaire;
CREATE SCHEMA vaultaire AUTHORIZATION vaultaire;

-- Run migrations
vaultaire migrate up
Monitoring
Prometheus Configuration
yaml# prometheus.yml
scrape_configs:
  - job_name: 'vaultaire'
    static_configs:
      - targets:
        - 'hub:9090'
        - 'spoke1:9090'
        - 'spoke2:9090'
    metrics_path: '/metrics'
Grafana Dashboard
Import dashboard ID: vaultaire-12345 from grafana.com
Backup Strategy
Database Backup
bash# Daily backup
0 2 * * * pg_dump vaultaire | gzip > /backup/vaultaire-$(date +\%Y\%m\%d).sql.gz

# Retention: 30 days
find /backup -name "vaultaire-*.sql.gz" -mtime +30 -delete
Storage Backup
Vaultaire handles this automatically with replication policies.
Security Hardening
TLS Configuration
yamlserver:
  tls:
    enabled: true
    cert: /etc/vaultaire/tls/cert.pem
    key: /etc/vaultaire/tls/key.pem
    min_version: "1.2"
    ciphers:
      - TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384
      - TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
Firewall Rules
bash# Allow only necessary ports
ufw allow 22/tcp   # SSH
ufw allow 443/tcp  # HTTPS
ufw allow 9000/tcp # Vaultaire API
ufw allow 9090/tcp # Metrics (internal only)
ufw enable
API Rate Limiting
yamlrate_limiting:
  enabled: true
  requests_per_minute: 1000
  burst: 100
Scaling Guidelines
Vertical Scaling

Hub: Increase RAM for cache (256GB recommended)
Spokes: Increase CPU cores for concurrent requests

Horizontal Scaling

Add more spokes in different regions
Use GeoDNS for routing
Implement read replicas for database

Storage Scaling

Start with single backend
Add backends as needed
Use tiering for cost optimization

Troubleshooting
Common Issues
Port Already in Use
bash# Find process using port
lsof -i :9000
# Kill process
kill -9 <PID>
Permission Denied
bash# Fix permissions
chown -R vaultaire:vaultaire /var/lib/vaultaire
chmod 755 /var/lib/vaultaire
Out of Memory
yaml# Increase container limits
resources:
  limits:
    memory: 4Gi
  requests:
    memory: 2Gi
Health Checks
bash# Check service health
curl http://localhost:9000/health

# Check backend connectivity
vaultaire test backend --name primary

# Check database connection
vaultaire test database
Debug Mode
bash# Enable debug logging
VAULTAIRE_LOG_LEVEL=debug vaultaire serve

# Enable pprof profiling
VAULTAIRE_PPROF=true vaultaire serve
Migration Guide
From MinIO
bash# Export data
mc mirror minio/bucket /tmp/export

# Import to Vaultaire
vaultaire import \
  --source file:///tmp/export \
  --destination s3://vaultaire/container
From Ceph
bash# Use rclone
rclone sync ceph:bucket vaultaire:container
Support

Documentation: https://docs.vaultaire.io
Discord: https://discord.gg/vaultaire
Email: support@stored.ge
