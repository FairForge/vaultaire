# Client-side encryption

> The customer copy of this guide is the served page at https://stored.cloud/docs/encryption (`internal/api/docs_encryption.md`, its tokens filled from `internal/hosts` and `internal/api/landing/modes.json`; `TestEncryptionGuide_CopyRules` holds it to contract rule 8). This file is the repository copy: keep the two in step.

Three tools encrypt your files on your own computer before a byte is uploaded:
**restic**, **Kopia** and **rclone crypt**. The key is your password and never
leaves your device, so what reaches `https://s3.stored.cloud` is ciphertext. That is the one
case where we say it plainly: we cannot read what restic, Kopia or rclone crypt
encrypted on your device. The encryption is the tool's, not ours; what we add is
that we do not get in its way.

This page says what each tool hides, what we still see whichever tool you use,
and what we measured.

## The bucket

Use an ordinary bucket: a Standard bucket. The default for every bucket: nothing to set up. What that
means for the bucket itself: [the Standard row's customer sentence in `internal/api/landing/modes.json`; the served page prints the one that is true now — with or without the master key.] In a bucket written by one of
these tools, what we hold is the tool's ciphertext and its repository layout.

A Private bucket refuses every write that carries no SSE-C key
(`400 InvalidRequest`). None of the commands below sends one, so keep these
buckets Standard.

Lose the password and the backups are gone: the tool's key is derived from it,
and we hold no copy. Keep it in a password manager, and a second copy offline.

## restic

```
export AWS_ACCESS_KEY_ID=your_access_key
export AWS_SECRET_ACCESS_KEY=your_secret_key
export AWS_DEFAULT_REGION=us-east-1
export RESTIC_REPOSITORY=s3:https://s3.stored.cloud/my-backups
export RESTIC_PASSWORD_FILE=~/.config/restic/password

restic init
restic backup ~/Documents
restic check --read-data-subset=5%
restic restore latest --target ~/restore
restic forget --keep-daily 7 --keep-weekly 4 --prune
```

**What restic encrypts:** file contents, file and folder names, sizes,
timestamps and permissions — the whole tree is stored as encrypted blobs
(AES-256 in counter mode with a Poly1305-AES authenticator, the key derived
from your password with scrypt). Files are cut into chunks by content-defined
chunking (Rabin fingerprints, 512 KiB to 8 MiB, about 1 MiB on average, with a
polynomial chosen for your repository) and the chunks are packed, encrypted,
into pack files of about 16 MiB. Chunk boundaries sit inside the encrypted
packs.

**What restic leaves visible:** the repository layout — `config`, `keys/`,
`snapshots/`, `index/` and `data/<aa>/<sha256>` objects, each named by the hash
of its own ciphertext — so the number and size of the packs, how much each
backup adds, and how many snapshots you keep.

## Kopia

```
kopia repository create s3 \
  --bucket=my-backups \
  --endpoint=s3.stored.cloud \
  --region=us-east-1 \
  --access-key=your_access_key \
  --secret-access-key=your_secret_key

kopia snapshot create ~/Documents
kopia snapshot verify --verify-files-percent=5
kopia restore <snapshot-id> ~/restore
```

**What Kopia encrypts:** file contents, and the directory listings that carry
names, sizes and times, with `AES256-GCM-HMAC-SHA256` by default (the key
derived from your password with scrypt). Files are cut by a content-defined
splitter (`DYNAMIC-4M-BUZHASH`, about 4 MiB on average) and the pieces packed,
encrypted, into blobs of up to about 21 MB.

**What Kopia leaves visible:** the blob layout — `p…` data packs, `q…`
metadata packs, `xn…` indexes, `_log_…` session logs — so the volume of data
against metadata and the maintenance cadence; and one plaintext object by
design, `kopia.repository`, the format blob: the tool, its version, the
algorithm names, a random repository id and an encrypted key block. No file
data, no names.

## rclone crypt

```
rclone config create stored s3 \
  provider=Other \
  access_key_id=your_access_key \
  secret_access_key=your_secret_key \
  endpoint=https://s3.stored.cloud \
  region=us-east-1

rclone config create secret crypt \
  remote=stored:my-backups \
  password='a long passphrase' \
  password2='a second, different passphrase'

rclone copy ~/Documents secret: -P \
  --s3-upload-cutoff 64M --s3-chunk-size 64M
```

`rclone config create` obscures both passphrases in the config file; `password2`
is the salt — set it, and keep both.

**What rclone crypt encrypts:** each file's contents (XSalsa20-Poly1305 in
64 KiB blocks behind a 32-byte header with a random nonce), and by default
each file and folder name (AES-EME).

**What rclone crypt leaves visible:** it keeps one object per file, so we see
how many files you have and how the folders nest; each file's size to within a
few bytes (the ciphertext is the plaintext plus 32 bytes plus 16 bytes per
64 KiB); roughly how long each name is; and each file's modification time,
which rclone stores beside the object as metadata. There is no deduplication
between files. If those are part of your threat model, restic or Kopia hide
more.

## What we still see, whichever tool

- your account, your bucket names and when you created them;
- how many objects each bucket holds, their names as the tool writes them
  (hashes, or encrypted names) and their sizes;
- when each object is written, read and deleted, and from which address the
  requests come;
- how much you store (what we bill) and how much you download (what your
  monthly allowance counts).

So the true sentence is "we hold ciphertext", never "we see nothing".

## What we measured

On 2026-11-01 we ran both restic and Kopia through their whole lifecycle
against the live service, through Cloudflare and our public endpoint — the path
`https://s3.stored.cloud` takes — with nothing set beyond the endpoint and the credentials.
The dataset was 3,043 files, 713.7 MiB (documents, photos, videos), each file
starting with a marker string.

| Tool | Steps | Result | Objects stored | Objects with the marker or a file name |
|---|---|---|---|---|
| restic 0.18.1 | init, backup, check, check `--read-data`, restore, incremental backup, `forget --prune`, check | every step passed; the restore matched all 3,043 SHA-256 hashes | 52 | 0 |
| Kopia 0.23.1 | repository create, snapshot, verify 100 %, restore, incremental snapshot, snapshot delete + full maintenance, verify | every step passed; the restore was identical to the source | 46 | 0 |

**0 of 98 stored objects contained the marker or a file name.** The one
plaintext object was Kopia's `kopia.repository` format blob, described above.
rclone crypt was not part of that run.

## Limits

- **One upload request carries at most 100 MB** (Cloudflare's limit). restic's
  default packs (about 16 MiB) and Kopia's (up to about 21 MB) are far under it;
  if you raise restic's `--pack-size`, stay under 95 MiB. rclone: keep the
  upload cutoff and chunk size at 64M, as above.
- **No server-side deduplication.** Encrypted bytes do not repeat, so we cannot
  deduplicate them; restic and Kopia deduplicate inside their own repository,
  which is better for backups anyway.
- **Egress is never billed.** Restores and `check --read-data` count toward your
  monthly download allowance like any other download.

Questions? [FAQ](https://stored.cloud/docs/faq) · [support@stored.cloud](mailto:support@stored.cloud)
