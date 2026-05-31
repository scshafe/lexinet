# lexinet Docker Deploy

This directory mirrors the loose_ends deploy shape: one multi-stage image for the single `lexinet` binary plus a Compose stack for the HTTP service and its Postgres database.

## Start

```sh
docker compose -f deploy/docker/docker-compose.yml up --build
```

The service listens on `http://127.0.0.1:17892`. On startup, `lexinet serve` applies the embedded migrations unless `--skip-migrate` is supplied.

## Configuration

`deploy/docker/config.toml` is mounted at `/etc/lexinet/config.toml` and selected with `LEXINET_CONFIG`. The Compose database DSN is:

```toml
postgres://lexinet:lexinet@postgres:5432/lexinet?sslmode=disable
```

For a real deployment, change the Postgres password and set `service.auth_token` to require `Authorization: Bearer <token>` on all routes except `/healthz` and `/version`. CLI clients can read their token from `server.token_file`; token files intentionally take precedence over `LEXINET_TOKEN`.

## Tailnet Listener

The Docker profile keeps tailnet disabled with `--tailnet off` so the build remains safe and self-contained. To enable the tsnet listener in an environment that includes the real Tailscale dependency, set `service.tailnet_enabled = true`, choose `service.tailnet_hostname`, persist `service.tailnet_state_dir`, and start without `--tailnet off` or with `--tailnet on`.

## Validation

After the stack is healthy:

```sh
lexinet --endpoint http://127.0.0.1:17892 ping --json
lexinet --endpoint http://127.0.0.1:17892 idea list --json
```

Host-side DB validation should include migration application, creating an idea with names and bodies, resolving by alias/acronym, relationship traversal, full-text search, fuzzy search, and unknown-field rejection.
