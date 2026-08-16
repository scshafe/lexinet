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

For a real deployment, change the Postgres password and set `service.auth_token` to require `Authorization: Bearer <token>` on all routes except `/healthz` and `/version`. CLI clients can read their token from `server.token_file`; token files intentionally take precedence over `LEXINET_TOKEN`. The built-in and Docker service configurations explicitly disable Lexinet's native Tailnet listener. The Docker process still listens on `0.0.0.0` inside its container, so its empty development bearer is safe only while the host mapping remains loopback-only and no external proxy targets it.

## Tailnet Listener

The Docker profile keeps Lexinet's native Tailnet listener disabled with `--tailnet off`. To enable the tsnet listener in an environment that includes the real Tailscale dependency, first set a nonempty, non-whitespace `service.auth_token`; then set `service.tailnet_enabled = true`, choose `service.tailnet_hostname`, persist `service.tailnet_state_dir`, and start without `--tailnet off` or with `--tailnet on`. Lexinet rejects native Tailnet enablement without that bearer before migrations or service startup.

That guard does not observe a host-side TCP proxy. The `tailscale serve --bg --tcp 17892 tcp://127.0.0.1:17892` path noted in `docker-compose.yml` can expose the Tailnet-disabled Docker service while Lexinet still sees an ordinary loopback connection. Set `service.auth_token` before enabling that proxy; Step 15a does not add proxy identity, TLS, or capability scoping.

## Validation

After the stack is healthy:

```sh
lexinet --endpoint http://127.0.0.1:17892 ping --json
lexinet --endpoint http://127.0.0.1:17892 idea list --json
```

Host-side DB validation should include migration application, creating an idea with names and bodies, resolving by alias/acronym, relationship traversal, full-text search, fuzzy search, and unknown-field rejection.
