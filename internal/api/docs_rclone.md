# rclone + stored.ge

rclone is the fastest way to sync, mount, or migrate data to stored.ge.

## Quick config (one command)

```
rclone config create stored s3 \
  provider=Other \
  access_key_id=your_access_key \
  secret_access_key=your_secret_key \
  endpoint=https://stored.ge \
  region=us-east-1
```

That's the whole setup. Verify it:

```
rclone lsd stored:          # list buckets
rclone mkdir stored:backups # create a bucket
```

## Interactive config (if you prefer prompts)

Run `rclone config` → `n` (new remote) → name it `stored` →
storage type `s3` → provider `Other` → enter your access key and secret →
region `us-east-1` → endpoint `https://stored.ge` → accept defaults for the rest.

## Everyday commands

```
# Copy a folder up (with progress)
rclone copy ~/backups stored:backups -P

# Sync (mirror — deletes remote files no longer local)
rclone sync ~/backups stored:backups -P

# Mount as a local filesystem
rclone mount stored:backups ~/mnt/stored --vfs-cache-mode writes

# Serve over HTTP/WebDAV/SFTP
rclone serve webdav stored:backups --addr :8080
```

## Recommended flags for large transfers

```
rclone copy ~/data stored:data -P \
  --transfers 16 \
  --s3-chunk-size 64M \
  --s3-upload-concurrency 8
```

stored.ge streams uploads chunk-by-chunk, so large objects don't buffer in memory
on either end. From a home connection you'll saturate your uplink first.

## Client-side encryption (optional)

Want encryption we can never undo? Wrap the remote with `rclone crypt` — only you hold the key:

```
rclone config create secret crypt remote=stored:backups \
  password=YOUR_PASSWORD
```

Now use `secret:` instead of `stored:backups`. Note: client-side encryption
disables our server-side deduplication for that data (we can't dedupe bytes we
can't read) — everything else works normally.

## Notes

- Use **path-style** addressing (rclone's S3 `Other` provider does this by default).
- Egress is never billed: Standard includes free egress up to 0.5× your quota per
  month (Vault restores up to 1×), beyond that throughput may be throttled — so
  `rclone sync`-ing your data back out, or leaving entirely, costs nothing. No lock-in
  by design.
- Questions? [FAQ](/docs/faq) · [support@stored.ge](mailto:support@stored.ge)
