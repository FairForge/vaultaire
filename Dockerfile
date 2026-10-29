# The local server (WP-DX-1 PR C, plan 15.1 — the product's "stripe-mock"):
# the release binary of scripts/build-release.sh, as built, on a pinned
# distroless base. Nothing is compiled here: the binary in the image is
# byte-for-byte the one the transparency log's server_build entry names
# (the Deploy workflow checks the sha256), so `make docker` builds it first:
#
#   GOOS=linux GOARCH=<arch> scripts/build-release.sh bin/vaultaire-linux-<arch>
#
# Run it with Postgres: deploy/docker/compose.yml, docs/guides/local.md.
# STORAGE_MODE=local — objects on the container's volume, never a paid
# backend.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG TARGETARCH
COPY bin/vaultaire-linux-${TARGETARCH} /vaultaire
# /data owned by nonroot (no shell to mkdir with): a named volume mounted
# there starts from this directory, ownership included.
COPY --chown=nonroot:nonroot deploy/docker/data/ /data/
ENV PORT=8000 \
    STORAGE_MODE=local \
    DATA_PATH=/data \
    MULTIPART_STAGING_DIR=/data/.multipart
VOLUME ["/data"]
EXPOSE 8000
USER nonroot:nonroot
ENTRYPOINT ["/vaultaire"]
