-- 116_sandbox.sql
--
-- WP-DX-1 (Stage 1a Prompt 50, plan § WP-STRIPE — test mode). Additive
-- only: the previous build reads none of it.
--
-- tenants.sandbox_of: a tenant's SANDBOX — the test-mode tenant its
-- VLT_TEST_ keys resolve to — names its parent here. NULL on every live
-- tenant. One sandbox per tenant (the partial unique index); a sandbox
-- has no sandbox of its own (refused by the code that mints it). A
-- sandbox's writes go to the `local` driver only, are never billed or
-- metered, and the sandbox is erased with its parent (internal/account,
-- the sandbox's rows first).

ALTER TABLE tenants ADD COLUMN IF NOT EXISTS sandbox_of VARCHAR(255) REFERENCES tenants(id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_tenants_sandbox_of ON tenants (sandbox_of) WHERE sandbox_of IS NOT NULL;

-- events.livemode (WP-DX-1 PR B, Stripe's word): FALSE for an event of a
-- sandbox, which is recorded under — and delivered to the webhooks of —
-- its PARENT, with `livemode: false` in the body; every delivery attempt
-- (the retry job's too) builds the body from the row, so the mode is the
-- row's. Every existing event is live.
ALTER TABLE events ADD COLUMN IF NOT EXISTS livemode BOOLEAN NOT NULL DEFAULT TRUE;
