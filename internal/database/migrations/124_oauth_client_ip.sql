-- 124_oauth_client_ip.sql — Stage 1a Prompt 57 Part 0, RP92-5.
--
-- The MCP authorization server's per-day cap of client registrations
-- (RP90-3) was an in-memory counter keyed on the peer address: a slot
-- switch zeroed it, 20,001 addresses of one IPv6 /64 reset the whole map,
-- and the cap was spent before a registration was validated. The cap is
-- now a COUNT over this column inside the registration's transaction
-- (auth.RegisterOAuthClient): `created_ip <<= <the peer's /64 or /32>` and
-- `created_at > NOW() - interval '1 day'`, so only an inserted row spends
-- it. NULL = a client registered before 124 (counted by nothing).
--
-- The address is personal data kept as long as the cap needs it: the
-- retention job blanks it two days after the registration (produce.json's
-- agent_oauth_clients scrub row). The clients stay infrastructure: no
-- tenant column, outside the erasure walk and the export.
ALTER TABLE agent_oauth_clients ADD COLUMN IF NOT EXISTS created_ip INET;

-- The cap's scan: the day's registrations (a small set — 20 per network).
CREATE INDEX IF NOT EXISTS idx_agent_oauth_clients_created_at
    ON agent_oauth_clients (created_at) WHERE created_ip IS NOT NULL;
