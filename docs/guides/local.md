# Run Stored on your machine

The local server is the same binary that serves `stored.cloud`, built by the
one release recipe (`scripts/build-release.sh`), on a pinned distroless base.
It stores objects on its own volume (`STORAGE_MODE=local`) and never reaches
a paid backend. It has the same S3 API and the same OpenAPI as production.
Use it to try Stored before you have an account, and to run tests that must
not touch the network.

It needs Docker (or colima / OrbStack) and a checkout of this repository.
The compose file runs Postgres 18 next to the image and applies the
checkout's migrations at every `up`.

## The five commands

```bash
git clone https://github.com/FairForge/vaultaire && cd vaultaire
make docker                                    # stored/vaultaire:local from the release binary
docker compose -f deploy/docker/compose.yml up -d
docker compose -f deploy/docker/compose.yml logs vaultaire | grep AWS_   # the first-boot key pair
aws --endpoint-url http://localhost:8000 s3 cp ./file s3://my-bucket/    # after `aws s3 mb s3://my-bucket`
```

- **The first boot** makes one account (`dev@localhost.local`) and prints its
  key pair and a dashboard password **once**. Later boots print no secret.
  Lost them? Run `down -v` and start again, or sign in to the dashboard
  (`http://localhost:8000/dashboard`) and rotate them. The first-boot account
  is made only with `STORAGE_MODE=local` (the image's default).
- **Ports:** the server listens on `127.0.0.1:8000`. Set `STORED_PORT` to use
  another.
- **Data:** objects live in the `data` volume and the database in `pgdata`.
  `docker compose -f deploy/docker/compose.yml down -v` removes both.
- **The published image:** every production deploy pushes
  `ghcr.io/fairforge/vaultaire:<sha12>` and `:latest`. Its `/vaultaire` is
  byte-for-byte the binary production runs: the Deploy workflow checks it
  against the sha256 of the transparency log's `server_build` entry. Use it
  with `STORED_IMAGE=ghcr.io/fairforge/vaultaire:latest docker compose -f
  deploy/docker/compose.yml up -d`. The published image is linux/amd64;
  `make docker` builds your machine's own architecture.
- **Not for a server:** the JWT secret in the compose file is a placeholder
  and the database has no password.

Test mode on the hosted service (`VLT_TEST_` keys, a sandbox per account) is
the other way to try things without cost: `docs/API.md` § Test mode.
