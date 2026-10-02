# Mount Stored as a POSIX Filesystem with JuiceFS

Turn your Stored S3 bucket into a full POSIX filesystem you can `cd`, `ls`, and `cat` — on any Linux VPS with 512MB of RAM.

## What is JuiceFS

JuiceFS is an open-source, high-performance POSIX filesystem that splits storage into two layers: a metadata engine (SQLite, Redis, or PostgreSQL) and a data backend (any S3-compatible service). Your files look and behave like local files, but the bytes live in S3. It supports file locking, extended attributes, and symlinks — things FUSE-mounted rclone cannot do.

## Prerequisites

- Any Linux VPS (Ubuntu 20.04+, Debian 11+, or similar) with at least 512MB RAM
- A Stored account with your access key and secret key (find them in your dashboard at stored.ge)
- A bucket created on Stored (you'll do this below if you haven't already)

## Step 1: Install JuiceFS

```bash
curl -sSL https://d.juicefs.com/install | sh -
```

Verify the install:

```bash
juicefs version
```

## Step 2: Create a Bucket on Stored

If you haven't already, create a bucket using the AWS CLI:

```bash
aws s3 mb s3://my-jfs-data \
  --endpoint-url https://stored.ge \
  --region us-east-1
```

Or create one from your Stored dashboard.

## Step 3: Format the Filesystem

This tells JuiceFS how to reach your S3 data and where to store metadata. For a single-node setup, SQLite is the simplest metadata engine:

```bash
sudo mkdir -p /var/jfs

juicefs format \
  --storage s3 \
  --bucket https://stored.ge/my-jfs-data \
  --access-key VKxxxxxxxxxxxxxxxx \
  --secret-key SKxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx \
  sqlite3:///var/jfs/meta.db \
  myjfs
```

Replace `VKxxxxxxxxxxxxxxxx` and `SKxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx` with your actual Stored credentials — the primary pair shown once at sign-up, or a scoped `VLT_…` key from Dashboard → API Keys limited to this bucket. The last argument (`myjfs`) is your filesystem name.

## Step 4: Mount the Filesystem

```bash
sudo mkdir -p /mnt/storedge

juicefs mount sqlite3:///var/jfs/meta.db /mnt/storedge
```

JuiceFS will run in the foreground. To run it in the background, add `-d`:

```bash
juicefs mount -d sqlite3:///var/jfs/meta.db /mnt/storedge
```

## Step 5: Verify

```bash
echo "hello Stored" > /mnt/storedge/test.txt
cat /mnt/storedge/test.txt
ls -la /mnt/storedge/
```

You should see your file. It lives on Stored but behaves exactly like a local file.

## Automounting on Boot

Create a systemd unit so your filesystem mounts automatically:

```bash
sudo tee /etc/systemd/system/juicefs-storedge.service << 'EOF'
[Unit]
Description=JuiceFS mount for Stored
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/juicefs mount sqlite3:///var/jfs/meta.db /mnt/storedge --no-syslog
ExecStop=/usr/local/bin/juicefs umount /mnt/storedge
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now juicefs-storedge
```

Check status:

```bash
sudo systemctl status juicefs-storedge
```

## Performance Tips

**Enable read caching** to avoid re-downloading frequently accessed files:

```bash
juicefs mount -d \
  --cache-dir /tmp/jfscache \
  --cache-size 1024 \
  sqlite3:///var/jfs/meta.db /mnt/storedge
```

This caches up to 1GB of recently read data locally. On a VPS with spare disk, bump it higher.

**Prefetch for sequential reads** (media streaming, backups):

```bash
juicefs mount -d \
  --cache-dir /tmp/jfscache \
  --cache-size 2048 \
  --prefetch 3 \
  sqlite3:///var/jfs/meta.db /mnt/storedge
```

**Write buffering** is enabled by default — JuiceFS batches small writes into larger S3 uploads automatically.

**For multi-node setups**, replace SQLite with Redis:

```bash
juicefs format \
  --storage s3 \
  --bucket https://stored.ge/my-jfs-data \
  --access-key VKxxxxxxxxxxxxxxxx \
  --secret-key SKxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx \
  redis://localhost:6379/0 \
  myjfs
```

This lets multiple machines mount the same filesystem simultaneously with consistent metadata.
