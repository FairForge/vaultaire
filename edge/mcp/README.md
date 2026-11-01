# stored-mcp — Stored's MCP server (WP-AGENT-1)

A Cloudflare Worker on `mcp.stored.cloud` that lets an AI agent act on a Stored
account for the person who connected it (`docs/ACCOUNTS.md` § Agents). It holds
no secret and decides nothing: every call carries the **agent grant's own key**
(a management key) and the origin refuses what the grant may not do.

- `POST /mcp` — MCP over Streamable HTTP (JSON answers; no server stream).
  Without a bearer: `401` + `WWW-Authenticate: Bearer resource_metadata=…`.
- `GET /.well-known/oauth-protected-resource` — RFC 9728; names the origin's
  authorization server by the `issuer` its own metadata publishes (the
  canonical host — `stored.ge` on prod until WP-DOMAIN-1 moves it; a client
  refuses an issuer that differs from the URL it fetched, RFC 8414 § 3.3).
  `STORED_API` is only where the Worker reaches the origin.
- The origin runs OAuth 2.1 + PKCE (S256) with dynamic client registration:
  `/.well-known/oauth-authorization-server`, `/oauth/register`,
  `/oauth/authorize` → the dashboard's consent (a person, TOTP) →
  `/oauth/token`, which answers the grant's `sk_live_…` key (and its sandbox
  `sk_test_…` key as `stored_test_token`).

Tools: `list_buckets`, `create_bucket`, `delete_bucket` (asks for approval),
`list_objects`, `put_object` / `get_object` / `delete_object` (≤ 4 MiB; each
call mints an S3 key on that one bucket under the grant's ceiling and caps,
signs SigV4 with it, and revokes it before answering).

Local clients: `stored mcp` bridges stdio to this endpoint
(`STORED_TOKEN` = the grant's key).

```bash
node --test                 # make test-edge runs it in CI
npx -y wrangler@4.151.3 deploy
```
