-- 099: backend_plans — what we bought from each storage vendor (WP-CAP-1 A,
-- docs/IMPLEMENTATION_PLAN.md § WP-CAP). One row per registered backend,
-- written by the operator through PUT /api/v1/admin/backends/{backend}/plan.
-- Infrastructure, not tenant data: no tenant/user column, outside the
-- erasure walk and the export (internal/account/schema_test.go).
--
-- Why now: the original iDrive reseller account died on 2026-09-07 because
-- its free year ended with no card and nothing on our side knew a renewal
-- date existed (12 days unnoticed). The daily backend_capacity job reads
-- these rows beside what our tables say each backend holds, and the
-- capacity / renewal / egress alerts fire from them.

SET lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS backend_plans (
    -- the registered driver name (idrive, lyve, geyser, wasabi, r2, ...)
    backend                  TEXT        PRIMARY KEY,
    vendor                   TEXT        NOT NULL DEFAULT '',
    plan_name                TEXT        NOT NULL DEFAULT '',
    -- what the plan includes before overage (the 1 TB block)
    capacity_bytes           BIGINT      NOT NULL CHECK (capacity_bytes > 0),
    -- free egress per month as a multiple of stored bytes (iDrive: 3);
    -- NULL = the vendor does not meter egress
    egress_free_multiple     DOUBLE PRECISION CHECK (egress_free_multiple IS NULL OR egress_free_multiple >= 0),
    -- billed past the block (iDrive: $5.12/TB-month) and past the free egress
    overage_usd_per_tb_month NUMERIC(10, 2),
    egress_usd_per_gb        NUMERIC(10, 4),
    term_start               TIMESTAMPTZ NOT NULL,
    term_end                 TIMESTAMPTZ NOT NULL,
    auto_renew               BOOLEAN     NOT NULL DEFAULT FALSE,
    -- a card on file: BackendPlanNoPayment pages when this is false within
    -- 30 days of term_end (the 2026-08-27 shape)
    payment_on_file          BOOLEAN     NOT NULL DEFAULT FALSE,
    invoice_ref              TEXT        NOT NULL DEFAULT '',
    note                     TEXT        NOT NULL DEFAULT '',
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by               TEXT        NOT NULL DEFAULT '',
    CHECK (term_end > term_start)
);
