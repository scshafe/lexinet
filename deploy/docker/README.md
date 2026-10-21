# lexinet Docker Deploy

This directory contains one multi-stage Go 1.26.10 image for the single `lexinet`
binary plus a Compose stack for the HTTP service and its Postgres database. The
binary pins Tailscale v1.102.2 for its optional embedded-node transport.

## Start

```sh
docker compose -f deploy/docker/docker-compose.yml up --build
```

The host service listens on `http://127.0.0.1:17892`. On startup,
`lexinet serve` applies the embedded migrations unless `--skip-migrate` is
supplied.

## Configuration

`deploy/docker/config.toml` is mounted at `/etc/lexinet/config.toml` and selected with `LEXINET_CONFIG`. The Compose database DSN is:

```toml
postgres://lexinet:lexinet@postgres:5432/lexinet?sslmode=disable
```

For a real deployment, change the Postgres password and set
`service.auth_token` to protect legacy loopback routes and provide the dedicated
loopback `study.v1` operator credential. CLI clients can read their token from
`server.token_file`; token files intentionally take precedence over
`LEXINET_TOKEN`. `/healthz` and `/version` remain public on the loopback router.
The Docker process listens on `0.0.0.0` inside its container, so its empty
development bearer is safe only while the published host port remains
loopback-only and nothing proxies that port.

The built-in and Docker service configurations keep Lexinet's native Tailnet
listener disabled. No host-side Tailscale Serve or raw-TCP proxy is part of this
deployment shape.

## Native Tailnet study listener

The Compose profile keeps the listener disabled with `--tailnet off`. A
production-specific profile may enable it only after all of these inputs exist:

- a persistent, pre-enrolled `tailnet_state_dir` that is a real directory owned
  by the container's effective user and mode `0700`;
- a hostname for which MagicDNS and Tailnet HTTPS can issue a native
  certificate;
- exactly one configured authentication mode; and
- the real tag, controlled-domain app-capability name, learner profile ceiling,
  and Tailnet grant (node mode), or an independent subject UUID and owner-only
  token file (scoped-token mode).

Do not mount a fresh empty/root-owned named volume and expect Lexinet to enroll
it. Provision the identity state outside this repository over a trusted path,
then bind-mount it at `/var/lib/lexinet/tsnet-state`. Treat that state as a
secret. Do not supply an auth key, OAuth secret, workload credential, alternate
control URL, or debug environment file through Compose environment variables,
command arguments, URLs, Git, logs, or diagnostics; startup rejects those
ambient enrollment/control-plane inputs.

Node-capability mode example (every value containing `replace` or `.example`
is a placeholder):

```toml
[service]
tailnet_enabled = true
tailnet_hostname = "lexinet"
tailnet_state_dir = "/var/lib/lexinet/tsnet-state"

[service.tailnet_study_authentication]
mode = "tailnet_node"
required_node_tag = "tag:replace-with-panel-study-tag"
application_capability = "operator-controlled.example/cap/lexinet-study-v1"
allowed_profile_refs = ["replace.with.profile"]
```

The caller must have exactly that singleton tag. Its connection-scoped Tailnet
grant must allow TCP 443 to the separately tagged Lexinet node and attach the
same app capability with only `create_or_resume` and `sync` for the intended
profile references. Lexinet performs fresh `WhoIs` authentication per request
and applies the configured operation/profile ceiling locally.

Scoped-token mode is an alternative, not a fallback:

```toml
[service.tailnet_study_authentication]
mode = "scoped_token"
allowed_profile_refs = ["replace.with.profile"]
scoped_token_subject_id = "11111111-1111-1111-1111-111111111111" # replace
scoped_token_file = "/run/secrets/lexinet-study-token"
```

Omit the tag/app-cap fields in this mode. The absolute token file must be
regular, owned by the service's effective user, and mode `0400` or `0600`; mount
it from a secret store, never from this repository. The subject UUID is
nonsecret, independently generated identity and must never be derived from the
token. Lexinet loads the file once before migrations and retains only a digest.

The native listener is fixed at HTTPS port 443 and has a separate router. It
exposes only study create/resume and sync. Study GET, vocabulary, operator,
health, and version routes remain on loopback port 17892. The Tailnet listener
does not use proxy identity/capability headers and does not fall back to raw TCP
or cleartext.

Revocation is mode-specific. In node mode, remove the app grant or source tag,
wait for Tailnet policy propagation, and verify the next request is rejected. In
scoped-token mode, stop or isolate the service, atomically replace the owner-only
file (and subject UUID when a new principal is intended), and restart; deleting
the file after startup does not erase the retained digest. Revoke the Lexinet
embedded node itself through Tailnet administration when its persisted state is
suspect.

Do not enable this deployment until sanitized real-node evidence covers tagged,
untagged, wrong-tag, live-revocation, and native certificate/hostname/handshake
cases. The house-panel side also needs an explicit choice between an official
SHA-256-pinned ARM static Tailscale archive and a pinned source-toolchain build.
Neither unit tests nor the disabled Compose profile satisfy those external
gates.

## Validation

After the stack is healthy:

```sh
lexinet --endpoint http://127.0.0.1:17892 ping --json
lexinet --endpoint http://127.0.0.1:17892 idea list --json
```

Host-side DB validation should include migration application, creating an idea with names and bodies, resolving by alias/acronym, relationship traversal, full-text search, fuzzy search, and unknown-field rejection.
