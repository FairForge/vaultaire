# Move from Backblaze B2 to Stored

Stored copies your B2 bucket for you: no `aws s3 sync` on your own machine,
no download and re-upload over your connection. You make a read-only key
in B2, start a transfer, and Stored copies every file, resumes where it
stopped, and skips what is already here.

## 1. Make a read-only key in B2

In the B2 web console: **Application Keys → Add a New Application Key**:

- **Allow access to bucket:** the bucket you are moving.
- **Type of access:** **Read Only**.
- Name it e.g. `stored-transfer`.

Or with the `b2` command-line tool:

```sh
b2 key create --bucket my-b2-bucket stored-transfer listBuckets,listFiles,readFiles
```

Note the **keyID** and the **applicationKey** (shown once). Note your
bucket's **S3 endpoint** too: B2 shows it on the bucket's page, e.g.
`s3.us-west-004.backblazeb2.com` — the region is the part after `s3.`
(`us-west-004`).

## 2. Make the bucket in Stored

In the dashboard, **Buckets → New bucket**, a Standard bucket (a transfer
cannot write into a Private bucket: it holds no key of yours). Make sure
your storage is large enough for what you are moving: a transfer pauses when
it is full.

## 3. Start the transfer

In the dashboard, **Transfers → Start a transfer**: the endpoint
(`https://s3.us-west-004.backblazeb2.com`), the region, your B2 bucket, an
optional prefix, the keyID and the applicationKey, and the Stored bucket.

Or through the management API, signed in as the account's owner or an admin
(a management key cannot start a transfer):

```sh
curl -X POST https://stored.cloud/api/v1/manage/transfers \
  -H "Authorization: Bearer $STORED_TOKEN" -H "Content-Type: application/json" \
  -d '{
    "bucket": "photos",
    "source": {
      "endpoint": "https://s3.us-west-004.backblazeb2.com",
      "region": "us-west-004",
      "bucket": "my-b2-bucket",
      "access_key": "004a…",
      "secret_key": "K004…"
    }
  }'
```

Stored lists your B2 bucket once before it accepts the transfer, so a wrong
key or endpoint is an immediate `400` (`source_denied` names B2's answer).

## 4. Watch it, and finish

The Transfers page (or `GET /api/v1/manage/transfers/{id}`) shows what was
listed, copied, already here, and failed. Files keep their names, content
types and metadata. When the last file is copied the transfer is
`completed`, your key is deleted from Stored's database, and a
`transfer.completed` event goes to your webhooks.

Then **delete the read-only key in B2** — you no longer need it, and a
database backup taken while the transfer ran still holds it until that
backup ages out (up to 30 days). Point your applications at Stored
(`https://s3.stored.cloud`, your Stored access key) and, once you are sure,
delete the B2 bucket.

**Running it again** is safe: a second transfer of the same bucket skips
every file already copied and copies only what is new or changed.

**What B2 charges:** reading from B2 is B2's download traffic, priced by B2
(free up to three times what you store there, a month, at the time of
writing). Large buckets move at up to 25 MiB/s per transfer.

**Large files:** a file larger than what the transfer's rate moves in about
a minute — 1.4 GiB at the default 25 MiB/s — is not copied yet. It is marked
`failed` with that reason, before any of it is read, and the rest goes on;
copy those files with an S3 client (`rclone copy`, `aws s3 cp`) for now.
