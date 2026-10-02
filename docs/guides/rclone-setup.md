# How to Use Stored with rclone

Connect rclone to Stored for file syncing, mounting, and serving — works on Linux, macOS, and Windows.

> The customer copy of this guide is the served page at https://stored.ge/docs/rclone (`internal/api/docs_rclone.md`); keep the two in step.

## What is rclone

rclone is a command-line tool for managing files on cloud storage. It supports over 70 backends, including any S3-compatible service. Think of it as rsync for the cloud — you get `copy`, `sync`, `mount`, and even `serve` (WebDAV, SFTP, FTP) against your Stored buckets.

## Step 1: Install rclone

```bash
# Linux / macOS
curl https://rclone.org/install.sh | sudo bash

# Or via package manager
# Ubuntu/Debian: sudo apt install rclone
# macOS: brew install rclone
# Windows: choco install rclone
```

Verify:

```bash
rclone version
```

## Step 2: Configure Stored Remote

Run the interactive config:

```bash
rclone config
```

Follow the prompts:

```
n) New remote
name> storedge
Storage> s3
provider> Other
env_auth> false
access_key_id> VKxxxxxxxxxxxxxxxx
secret_access_key> SKxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
region> us-east-1
endpoint> https://stored.ge
location_constraint> (leave blank, press Enter)
acl> private
Edit advanced config?> n
```

This creates the following entry in `~/.config/rclone/rclone.conf`:

```ini
[storedge]
type = s3
provider = Other
access_key_id = VKxxxxxxxxxxxxxxxx
secret_access_key = SKxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
region = us-east-1
endpoint = https://stored.ge
acl = private
```

You can also create this file directly instead of using the interactive setup.

The `VK…`/`SK…` pair is the primary key shown once at sign-up. A scoped `VLT_…` key from Dashboard → API Keys (per bucket, per operation, optional IP allowlist and expiry) works in exactly the same place and is the better choice for a machine that only needs one bucket.

## Step 3: Basic Operations

**List buckets:**

```bash
rclone lsd storedge:
```

**List files in a bucket:**

```bash
rclone ls storedge:my-bucket
```

**Copy files to Stored:**

```bash
# Single file
rclone copy ./report.pdf storedge:my-bucket/reports/

# Entire directory
rclone copy ./photos storedge:my-bucket/photos/ --transfers 8
```

**Sync a directory** (make remote match local, deleting extra files on remote):

```bash
rclone sync ./local-folder storedge:my-bucket/backup/ --transfers 8
```

**Download files:**

```bash
rclone copy storedge:my-bucket/photos/ ./downloaded-photos/
```

**Delete a file:**

```bash
rclone delete storedge:my-bucket/old-file.txt
```

## Step 4: Mount as a Filesystem

Mount your Stored bucket as a local directory:

```bash
mkdir -p /mnt/storedge

rclone mount storedge:my-bucket /mnt/storedge \
  --vfs-cache-mode full \
  --vfs-cache-max-size 10G \
  --dir-cache-time 5m \
  --daemon
```

Now you can use `/mnt/storedge` like a normal directory:

```bash
ls /mnt/storedge/
cp file.txt /mnt/storedge/
```

To unmount:

```bash
fusermount -u /mnt/storedge
```

## Step 5: Serve via WebDAV, SFTP, or FTP

Turn your Stored bucket into a WebDAV server accessible from any file manager:

```bash
rclone serve webdav storedge:my-bucket --addr :8080
```

Access it at `http://your-server:8080` from any WebDAV client (Windows Explorer, macOS Finder, Cyberduck).

**SFTP server:**

```bash
rclone serve sftp storedge:my-bucket --addr :2222 --user myuser --pass mypass
```

**FTP server:**

```bash
rclone serve ftp storedge:my-bucket --addr :2121 --user myuser --pass mypass
```

This is useful for giving access to people who don't have rclone or S3 clients.

## Automounting on Boot

Create a systemd unit:

```bash
sudo tee /etc/systemd/system/rclone-storedge.service << 'EOF'
[Unit]
Description=rclone mount for Stored
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/bin/rclone mount storedge:my-bucket /mnt/storedge \
  --vfs-cache-mode full \
  --vfs-cache-max-size 10G \
  --dir-cache-time 5m \
  --allow-other
ExecStop=/bin/fusermount -u /mnt/storedge
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now rclone-storedge
```

Make sure `/etc/fuse.conf` has `user_allow_other` uncommented for `--allow-other` to work.

## Performance Tips

**Increase parallelism** for large transfers:

```bash
rclone copy ./data storedge:my-bucket/ \
  --transfers 16 \
  --checkers 8 \
  --s3-upload-concurrency 4
```

**VFS cache modes** (for mount):

| Mode | Behavior | Best for |
|------|----------|----------|
| `off` | No caching, direct S3 reads | Rare access, save disk |
| `minimal` | Cache open files only | Light usage |
| `writes` | Cache writes, stream reads | Write-heavy workloads |
| `full` | Cache everything | General use, media streaming |

**Bandwidth limiting** (useful on metered connections):

```bash
rclone copy ./data storedge:my-bucket/ --bwlimit 50M
```

**Dry run** before destructive syncs:

```bash
rclone sync ./local storedge:my-bucket/ --dry-run
```

This shows what would be uploaded/deleted without actually doing it.
