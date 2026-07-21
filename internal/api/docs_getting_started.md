# Getting Started with stored.ge

stored.ge is S3-compatible object storage. If a tool speaks S3 — aws-cli, rclone,
restic, boto3, JuiceFS, Cyberduck — it works here. This guide gets you from signup
to your first upload in about two minutes.

## 1. Get your credentials

Your **Access Key ID** and **Secret Access Key** are shown once, right after you
sign up. Copy them then — we hash the secret and can't show it again.

Lost the secret? No problem: open **Dashboard → API Keys** and generate a new
key. Old keys keep working until you revoke them.

## 2. Your connection settings

| Setting | Value |
|---------|-------|
| Endpoint | `https://stored.ge` |
| Region | `us-east-1` |
| Access Key ID | from step 1 |
| Secret Access Key | from step 1 |
| Addressing | Path-style |

There are no API/request fees and no minimum storage duration. Your free tier
includes 5 GB — no credit card required.

## 3. Configure the AWS CLI

```
aws configure
# AWS Access Key ID:     your_access_key
# AWS Secret Access Key: your_secret_key
# Default region name:   us-east-1
# Default output format: json
```

Then create a bucket and upload a file:

```
aws --endpoint-url https://stored.ge s3 mb s3://my-first-bucket
echo "hello stored.ge" > hello.txt
aws --endpoint-url https://stored.ge s3 cp hello.txt s3://my-first-bucket/
aws --endpoint-url https://stored.ge s3 ls s3://my-first-bucket/
```

That's it — your file is stored, encrypted at rest, and tiered automatically.

## 4. Use it from your language of choice

**Python (boto3):**

```python
import boto3
s3 = boto3.client(
    "s3",
    endpoint_url="https://stored.ge",
    region_name="us-east-1",
    aws_access_key_id="your_access_key",
    aws_secret_access_key="your_secret_key",
)
s3.upload_file("hello.txt", "my-first-bucket", "hello.txt")
```

**Backup tools:** see the one-page [rclone guide](/docs/rclone). restic works
out of the box: `restic -r s3:https://stored.ge/my-bucket init`.

## 5. What happens to your data

- **Encrypted at rest** by default (SSE-S3). Want to hold your own keys? Use SSE-C.
- **Auto-tiered**: hot for the first ~30 days on fast enterprise S3, then migrated
  toward tape-backed archive. You don't configure lifecycle rules unless you want to.
- **Deduplicated and compressed** transparently — you're billed for logical bytes.

## Next steps

- [rclone setup](/docs/rclone) — sync, mount, and migrate
- [FAQ](/docs/faq) — pricing, trust, and technical details
- [Pricing](/#pricing) · [Status](/status) · [support@stored.ge](mailto:support@stored.ge)
