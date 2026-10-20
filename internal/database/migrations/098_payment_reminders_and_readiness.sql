-- 098: payment reminders + persisted provider readiness (WP-PAY-3,
-- docs/PAYMENTS.md § 3.8 and § 7; Stage 1a Prompt 20).
--
-- entitlements.reminders_sent    which renewal reminders went out for which
--                                period: {"<step>@<paid_through RFC3339>":
--                                "<sent_at RFC3339>"} — a step is sent once
--                                per period, and a new paid_through (a
--                                renewal) resets every step by itself.
-- payment_provider_readiness     one row per payment provider: when it was
--                                last seen ready / unready and why (RP21-1):
--                                the readiness log the lapse job reads is no
--                                longer only in memory, so a restart does not
--                                forget an outage and its 24 h window is a
--                                real 24 h. Infrastructure, not tenant data:
--                                never erased with an account, never exported.
--                                A provider that is pending (configured,
--                                verification not completed) is never
--                                written.
--
-- Additive: the previous build ignores both during the two-slot switch.

SET lock_timeout = '5s';

ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS reminders_sent JSONB NOT NULL DEFAULT '{}'::jsonb;

CREATE TABLE IF NOT EXISTS payment_provider_readiness (
    provider        TEXT PRIMARY KEY,
    last_ready_at   TIMESTAMPTZ,
    last_unready_at TIMESTAMPTZ,
    last_reason     TEXT,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

RESET lock_timeout;
