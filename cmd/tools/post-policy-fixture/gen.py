#!/usr/bin/env python3
"""Generate internal/api/testdata/post_policy_boto3.json (WP-POST-1).

What boto3's generate_presigned_post produces for a fixed credential, date
and key — the reference a browser upload form is built from. The clock is
frozen so the output is reproducible byte for byte; the policy expires in
2099 so the checked-in fixture keeps verifying.

Run once, with a pinned boto3 in a throwaway virtualenv:

    python3 -m venv /tmp/boto && /tmp/boto/bin/pip install 'boto3==1.35.99'
    /tmp/boto/bin/python cmd/tools/post-policy-fixture/gen.py \
        > internal/api/testdata/post_policy_boto3.json

Not part of any build; never linked into the product.
"""
import datetime as _dt
import json
import sys

import boto3
import botocore
import botocore.auth
import botocore.signers

FROZEN = _dt.datetime(2026, 11, 3, 0, 0, 0)
EXPIRES = _dt.datetime(2099, 1, 1, 0, 0, 0)


class _FrozenDatetime(_dt.datetime):
    @classmethod
    def utcnow(cls):
        return FROZEN


class _Shim:
    """The datetime module as botocore sees it, with the clock frozen."""

    datetime = _FrozenDatetime
    timedelta = _dt.timedelta
    timezone = _dt.timezone


botocore.signers.datetime = _Shim
botocore.auth.datetime = _Shim

ACCESS_KEY = "VKPOSTPOLICYFIXTURE01"  # not an AKIA id: secret scanning
SECRET_KEY = "post-policy-fixture-secret-0123456789"
REGION = "us-east-1"
BUCKET = "post-fixture"
KEY = "uploads/${filename}"

s3 = boto3.client(
    "s3",
    aws_access_key_id=ACCESS_KEY,
    aws_secret_access_key=SECRET_KEY,
    region_name=REGION,
    endpoint_url="https://s3.stored.cloud",
    config=botocore.config.Config(signature_version="s3v4", s3={"addressing_style": "path"}),
)
post = s3.generate_presigned_post(
    BUCKET,
    KEY,
    Fields={"Content-Type": "image/png", "success_action_status": "201", "x-amz-meta-origin": "form"},
    Conditions=[
        ["starts-with", "$Content-Type", "image/"],
        {"success_action_status": "201"},
        ["content-length-range", 1, 1048576],
        {"x-amz-meta-origin": "form"},
    ],
    ExpiresIn=int((EXPIRES - FROZEN).total_seconds()),
)
json.dump(
    {
        "generator": f"boto3 {boto3.__version__} / botocore {botocore.__version__} generate_presigned_post",
        "frozen_now": FROZEN.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "access_key": ACCESS_KEY,
        "secret_key": SECRET_KEY,
        "region": REGION,
        "bucket": BUCKET,
        "url": post["url"],
        "fields": post["fields"],
    },
    sys.stdout,
    indent=2,
    sort_keys=True,
)
sys.stdout.write("\n")
