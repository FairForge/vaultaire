-- 114_public_catalogue.sql
--
-- WP-EDGE-4 PR D (Stage 1a Prompt 48, plan 10.3 — the public-artefact
-- catalogue). Additive only: the previous build reads none of it.
--
-- public_artefacts: public content the OPERATOR seeded (a distribution's
-- cloud image, a release tarball) — never a tenant's upload. One row per
-- artefact: its name, where it came from, its licence, who added it, the
-- sha256 of the whole file.
-- public_chunks: the artefact's chunks as the frozen chunker cuts them
-- (crypto.DefaultChunker): the PLAIN sha256 of each — public by
-- construction (anyone holding the artefact computes it) — and its size.
-- The catalogue_filter job builds a Bloom filter of these hashes; a data-
-- plane host marks a chunk whose plain hash the filter holds `public`, and
-- the control plane references the ONE copy in the `_public` dedup scope
-- (global_content_index rows the seed tool wrote) instead of storing it
-- (contract line 5: `_public` is the only cross-tenant scope, and nothing a
-- tenant uploads enters it).
-- Infrastructure: no tenant column; never erased with an account, never in
-- an export.
CREATE TABLE IF NOT EXISTS public_artefacts (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    source_url  TEXT NOT NULL DEFAULT '',
    licence     TEXT NOT NULL DEFAULT '',
    added_by    TEXT NOT NULL DEFAULT '',
    sha256      TEXT NOT NULL,
    size_bytes  BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (sha256)
);
CREATE TABLE IF NOT EXISTS public_chunks (
    artefact_id  BIGINT NOT NULL REFERENCES public_artefacts(id) ON DELETE CASCADE,
    chunk_index  INTEGER NOT NULL,
    plain_hash   TEXT NOT NULL,
    size_bytes   BIGINT NOT NULL,
    PRIMARY KEY (artefact_id, chunk_index)
);
CREATE INDEX IF NOT EXISTS idx_public_chunks_plain_hash ON public_chunks (plain_hash);
