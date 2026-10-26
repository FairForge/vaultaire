-- 110_edge_admits_get.sql
--
-- WP-EDGE-1 Part B (Stage 1a Prompt 45, plan 40.10 slice 1 — the read
-- half): a GetObject admitted at the edge has its own edge_admits row (op
-- 'get': the object's address, the bytes the Worker will stream, the
-- client's IP and user agent; nothing reserved, nothing minted). Migration
-- 109 declared the op CHECK inline, so Postgres named it
-- edge_admits_op_check; it is re-declared with the new value.
--
-- Additive (the previous build never writes 'get') and idempotent on the
-- deploy's re-apply (DROP IF EXISTS, then ADD). The previous build's reaper
-- selects every expired pending row with no op filter and treats it as a
-- lost PUT: safe while both slots run only because the 20-minute admit TTL
-- outlasts the old slot's drain (no 'get' row written by the new slot
-- expires before the old slot has stopped) and because the collector's
-- blobNamedBy re-checks a queued address under the key's lock before any
-- delete (a 'get' row's storage_key is the LIVE object's blob, which its
-- head row names) — docs/DEPLOY.md § The edge.
ALTER TABLE edge_admits DROP CONSTRAINT IF EXISTS edge_admits_op_check;
ALTER TABLE edge_admits ADD CONSTRAINT edge_admits_op_check CHECK (op IN ('put', 'part', 'get'));
