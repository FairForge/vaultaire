# Use Cases for JuiceFS + stored.ge on Cheap VPS

Real setups you can run today on a $3/year VPS backed by stored.ge storage. Every example assumes you've already mounted stored.ge at `/mnt/storedge` following the [JuiceFS setup guide](juicefs-setup.md).

## Plex / Jellyfin Media Server

Stream your media library from stored.ge without filling up your VPS disk. A $3/year NAT VPS + a Vault1 plan ($4.99/month for 1TB) gives you a full media server for under $65/year.

```bash
# Point Jellyfin at the mount
sudo ln -s /mnt/storedge/media /var/lib/jellyfin/media

# Or configure in Jellyfin's dashboard:
# Dashboard → Libraries → Add Media Library
# Folder: /mnt/storedge/media/movies
```

Upload your media:

```bash
cp -r /path/to/movies /mnt/storedge/media/movies/
cp -r /path/to/shows /mnt/storedge/media/tv/
```

For best streaming performance, enable the local read cache:

```bash
juicefs mount -d \
  --cache-dir /tmp/jfscache \
  --cache-size 4096 \
  --prefetch 3 \
  sqlite3:///var/jfs/meta.db /mnt/storedge
```

**Cost math**: 500GB of media on Vault1 ($4.99/mo) + BuyVM $3.50/yr VPS = ~$63/year total. Comparable Plex cloud setups on Google Drive or Dropbox run $100+/year and can revoke API access at any time.

## Nextcloud External Storage

You have two options: S3 object storage directly, or JuiceFS mount as local storage.

**Option A: S3 objectstore (recommended for new installs)**

Add to your Nextcloud `config.php`:

```php
'objectstore' => [
    'class' => '\\OC\\Files\\ObjectStore\\S3',
    'arguments' => [
        'bucket' => 'nextcloud-data',
        'key'    => 'VK_YOUR_ACCESS_KEY',
        'secret' => 'SK_YOUR_SECRET_KEY',
        'hostname' => 's3.stored.ge',
        'port'   => 443,
        'use_ssl' => true,
        'use_path_style' => true,
        'region'  => 'us-east-1',
    ],
],
```

**Option B: JuiceFS mount as local external storage**

In the Nextcloud admin panel:

1. Go to **Apps** → enable **External storage support**
2. Go to **Settings** → **External storage**
3. Add a **Local** storage pointing to `/mnt/storedge/nextcloud-files`

```bash
sudo mkdir -p /mnt/storedge/nextcloud-files
sudo chown www-data:www-data /mnt/storedge/nextcloud-files
```

This approach gives you POSIX compatibility (file locking, symlinks) that the S3 backend doesn't support.

## Immich Photo Backup

Immich supports S3 as a storage backend natively. In your `.env` file:

```bash
UPLOAD_LOCATION=/mnt/storedge/immich-uploads

# Or use S3 directly:
# IMMICH_S3_ENABLED=true
# IMMICH_S3_BUCKET=immich-photos
# IMMICH_S3_ENDPOINT=https://s3.stored.ge
# IMMICH_S3_ACCESS_KEY=VK_YOUR_ACCESS_KEY
# IMMICH_S3_SECRET_KEY=SK_YOUR_SECRET_KEY
# IMMICH_S3_REGION=us-east-1
```

The JuiceFS mount path (`UPLOAD_LOCATION`) is simpler to set up and doesn't require Immich's S3 support. Both approaches work — choose S3 direct if you want Immich to handle uploads natively, or JuiceFS if you want filesystem-level access to the photos.

**Cost math**: 200GB of photos on Vault1 ($4.99/mo) is $60/year. Google One 200GB is $30/year but locks you into their ecosystem. Immich + stored.ge gives you full ownership.

## Git LFS / CI Artifacts

Store large binary assets (models, datasets, build artifacts) on stored.ge via Git LFS.

Configure your `.lfsconfig`:

```ini
[lfs]
  url = "https://s3.stored.ge"

[lfs "storage"]
  s3.bucket = git-lfs-artifacts
  s3.endpoint = https://s3.stored.ge
  s3.access_key_id = VK_YOUR_ACCESS_KEY
  s3.secret_access_key = SK_YOUR_SECRET_KEY
```

Or use the JuiceFS mount for CI artifact caching:

```bash
# In your CI script
ARTIFACT_DIR=/mnt/storedge/ci-artifacts/$CI_PIPELINE_ID

mkdir -p $ARTIFACT_DIR
cp -r build/output/* $ARTIFACT_DIR/

# Later stages pull from the same path
cp $ARTIFACT_DIR/app.bin /deploy/
```

Artifacts persist across CI runs without eating VPS disk space.

## Database Backups

Dump your PostgreSQL (or MySQL) database straight to stored.ge on a schedule:

```bash
#!/bin/bash
# /usr/local/bin/backup-db.sh

BACKUP_DIR=/mnt/storedge/backups/postgres
DATE=$(date +%Y-%m-%d_%H%M)
FILENAME="mydb_${DATE}.sql.gz"

mkdir -p "$BACKUP_DIR"

pg_dump -U myuser mydb | gzip > "$BACKUP_DIR/$FILENAME"

# Keep 30 days of backups
find "$BACKUP_DIR" -name "*.sql.gz" -mtime +30 -delete

echo "Backup complete: $FILENAME ($(du -h "$BACKUP_DIR/$FILENAME" | cut -f1))"
```

Add a cron job:

```bash
# Run daily at 3am
echo "0 3 * * * /usr/local/bin/backup-db.sh >> /var/log/db-backup.log 2>&1" | crontab -
```

Your backups are stored off-server on stored.ge automatically. No rsync, no separate S3 upload step — `pg_dump` writes directly to the mount.

## *arr Stack (Sonarr / Radarr / Lidarr)

The *arr apps download media locally and then you move finished files to stored.ge for long-term storage. Don't point the download client directly at JuiceFS — downloading to a remote mount is slow.

**Setup:**

```bash
# Download to local SSD (fast)
# /downloads is on the VPS local disk

# Final media storage on stored.ge
mkdir -p /mnt/storedge/media/movies
mkdir -p /mnt/storedge/media/tv
mkdir -p /mnt/storedge/media/music
```

In Sonarr/Radarr settings:

- **Download client** category path: `/downloads/complete`
- **Root folder** for libraries: `/mnt/storedge/media/tv` (Sonarr) or `/mnt/storedge/media/movies` (Radarr)

The *arr apps will hardlink or move completed downloads to the JuiceFS mount. For best performance, use **Copy** instead of **Hardlink** in the import settings (hardlinks don't work across filesystem boundaries):

Sonarr/Radarr → Settings → Media Management:
- **Use Hardlinks instead of Copy**: No
- **Import using Script**: No

This gives you the speed of local downloads with the storage capacity of stored.ge. A 4TB Vault3 plan ($9.99/mo) holds a serious media library at a fraction of what a large VPS disk costs.
