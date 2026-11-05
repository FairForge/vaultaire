-- Migration 129: how long a request took (WP-LOG-2, D-LOG-4;
-- docs/reviews/WP-LOG-2.md). 128 is Prompt 60's (WP-EGRESS-1 Part 0).
--
-- duration_ms on both customer logs: the request log (115) and the bucket
-- access log (s3_access_log). NULL = not measured — a row the previous
-- build wrote, a row of a transfer whose commit never came (EdgeNoCommit),
-- or an edge commit whose host sent none. The origin measures from the top
-- of the S3 dispatcher (and the management middleware) to the deferred
-- write; an edge row carries the data-plane host's own duration_ms from its
-- commit. In the delivered access-log line it is the LAST field (`-` when
-- unknown); no earlier field moves.
--
-- Additive and idempotent: a nullable column with no default is a catalogue
-- change only (no rewrite), and the previous build's inserts, which name
-- their columns, leave it NULL. Erased and exported with the rows it is on.

ALTER TABLE request_log ADD COLUMN IF NOT EXISTS duration_ms INTEGER;
ALTER TABLE s3_access_log ADD COLUMN IF NOT EXISTS duration_ms INTEGER;
