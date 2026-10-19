# Deploying lexinet on the runner lane

Status (2026-10-18): **not deployed.** `dev.toml [deploy] lane = "none"`. The
runner lane's stack validator assumes a `ts-<node>` Tailscale sidecar, which
lexinet must not have (ADR 0003: the study listener is lexinet's own native
`tsnet.ListenTLS`, never behind a proxy or `serve`). `dev check` with a
sidecar-less `deploy/stack/` fails `V-06` ("exactly one tailscale/tailscale
service is required") and DEPLOY-05 ("sets no TS_HOSTNAME"). The draft stack is
in [`draft-stack/`](draft-stack/), outside `deploy/stack/` so `dev check` does
not see it. Issue #3 tracks the work.

- [enrollment.md](enrollment.md): enrolling the `lexinet-study` identity once.
- [data-migration.md](data-migration.md): moving the frozen stack's database.
- [`../validation/study-tailnet-release-gate.md`](../validation/study-tailnet-release-gate.md): the ADR 0003 evidence to record after deployment.

## Native-node requirements infra must support

For lexinet to move to the runner lane, the validator (`stack_validate.py`,
`stack_toml.py`) and the signed deploy entrypoint need a "native node" mode:

1. **No `ts-<node>` sidecar.** Accept a stack with zero `tailscale/tailscale`
   services (relax `V-06`, DEPLOY-05's `TS_HOSTNAME` check) when the stack
   declares a native node. The lexinet container joins the tailnet itself as
   `lexinet-study` (tag `tag:lexinet-study`) and is on the stack's ordinary
   network. Nothing is published on the host.
2. **`state/tsnet` is the node identity.** Today only the sidecar may mount
   `state/tailscale` (`V-07`). The native node mounts `./state/tsnet` read-write
   at `/var/lib/lexinet/tsnet-state` and nothing else may. Treat it like
   `state/tailscale`: included in the backup (`[backup] tailscale = true`
   equivalent, or `paths`), restored with the stack, never logged. Mode `0700`,
   owned by the container user (uid 65532); the deploy creates it that way. It
   is enrolled once, out of band, by `lexinet tailnet enroll`
   ([enrollment.md](enrollment.md)). `TS_AUTHKEY` must **never** reach the
   running service: lexinet refuses to start with it set.
3. **Health through the container healthcheck.** The native listener serves only
   study create/resume and sync (no `/healthz`, no `/version`), so
   `[deploy] health` cannot be probed "through the tailnet name". The
   deploy/`--verify` step must rely on the Docker healthcheck of the `lexinet`
   container (`lexinet --endpoint http://127.0.0.1:17892 ping`, loopback inside
   the container) plus the `lexinet-db` healthcheck, with no tailnet probe and
   no witness for this node. A tailnet-only check beyond that is the owner's
   release-gate evidence, not a deploy gate.
4. **No `serve.json`.** There is nothing for Serve to proxy; `V-13` must not
   require the file for a native node.
5. **The `db` service user.** `V-10` warns when a service sets no `user:`. The
   draft sets `user: "999:999"` (the `postgres` image's uid) on `lexinet-db`
   with `./state/db` owned by it; the deploy must chown that bind to the
   declared uid like it does for `65532`.
6. **Postgres backup.** `[backup] pg = { container, user, db }` exists; keep it
   for `lexinet-db`. Add `state/tsnet` to the backed-up paths as above.
7. **host.conf** grants: host binds `state/db` and `state/tsnet`; no loopback
   ports (nothing is published); a generated secret `LEXINET_DB_PASSWORD`.
   `V-12` still refuses `POSTGRES_HOST_AUTH_METHOD=trust`, so the database has a
   password.

These touch a security gate (the validator runs compose as root) and the signed
entrypoint, so they need a security review and the owner's hash approval.

### Open item on lexinet's side (for the stack PR)

lexinet reads its database DSN only from the config file or `--db`; it does not
read the password from the environment. The generated `LEXINET_DB_PASSWORD`
therefore cannot reach it yet. The stack PR must add an owner-only
`dsn_file` / password-file option (or `LEXINET_DATABASE_PASSWORD_FILE`) rather
than render the secret into `config.toml`. This is not done here.

### Decision recorded

No sidecar `lexinet` node and no tailnet operator API (owner, 2026-10-18): the
loopback API stays on `127.0.0.1` inside the container. The macOS menu-bar
client loses remote access; revisit later if wanted. The old
`mc-lexinet-enrichment` is retired.
