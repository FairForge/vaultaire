import { test } from "node:test";
import assert from "node:assert/strict";
import { signS3, objectPath } from "../src/sigv4.js";

// The vector is the Go AWS SDK v2 signer's (aws-sdk-go-v2 v4.Signer with DisableURIPathEscaping, as the S3 client signs) for the
// same request, computed in this repository on 2026-10-31.
test("signs a PUT exactly as the AWS SDK signs it", async () => {
  const body = new TextEncoder().encode("hello, agent");
  const h = await signS3({
    method: "PUT", url: "https://stored.ge/my-bucket/notes/a%20b.txt?x-id=PutObject", body,
    headers: { "content-length": String(body.length), "content-type": "text/plain" },
    accessKey: "VLTEXAMPLE", secretKey: "secretEXAMPLE", now: new Date(Date.UTC(2026, 9, 31, 12, 0, 0)),
  });
  assert.equal(h["x-amz-date"], "20261031T120000Z");
  assert.equal(h.authorization,
    "AWS4-HMAC-SHA256 Credential=VLTEXAMPLE/20261031/us-east-1/s3/aws4_request, SignedHeaders=content-length;content-type;host;x-amz-content-sha256;x-amz-date, Signature=e96196126875a806a0f560efc2e3105b82443b48904d4287bc195769ec0e2472");
});

test("an object path escapes each segment once", () => {
  assert.equal(objectPath("my-bucket", "notes/a b.txt"), "/my-bucket/notes/a%20b.txt");
});
