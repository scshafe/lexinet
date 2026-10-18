<!-- @canonical tailnet-study-access-decision -->

# Decision 0003: Scope Native Tailnet Access to the Study Appliance Surface

**Status:** accepted

**Date:** 2026-08-17

## Context

The house panel needs to create/resume a bounded `study.v1` session and sync its
locally queued evidence to Lexinet. Lexinet remains authoritative for study
content, ordering, adjudication, and scheduler provenance. The panel does not
need remote vocabulary administration, operator reads, health/version routes,
or an ambient path to the rest of Lexinet.

The earlier deployment description proposed reusing one handler on two
listeners and, in Docker notes, forwarding the loopback HTTP port through a raw
Tailscale Serve TCP proxy. That shape cannot bind application authorization to
the native peer connection. A shared bearer would also collapse all panels into
one authority and make tag or grant revocation irrelevant after the bearer was
copied.

Expected Tailnet, certificate, policy, and deployment failures must fail closed
with structured non-secret errors. Local tests can prove parsing and lifecycle
behavior, but cannot prove a real Tailnet policy, native certificate, MagicDNS
hostname, or panel package.

## Decision

Lexinet owns an embedded Tailscale node and a native HTTPS listener. Tailnet
access is a separate study-only trust boundary, not another way to reach the
loopback API.

### Transport and lifecycle

- The pinned toolchain is Go 1.26.5 with Tailscale v1.102.2.
- The only production Tailnet listener is
  `tsnet.ListenTLS("tcp", ":443")`. The port is fixed and there is no raw TCP,
  Funnel, cleartext, certificate fallback, proxy-header, or host-side Serve
  path.
- The embedded server reaches `Up`, obtains its LocalAPI client, and opens the
  TLS listener inside one bounded startup budget. Partial startup is closed and
  joined before an error returns.
- `PrepareConfiguration` validates transport and authorization before
  migrations. `ServePrepared` retains the prepared authority, opens both
  listeners, and shuts down and joins both HTTP servers when either listener or
  the parent context exits.
- The identity state directory must already exist and be pre-enrolled. It must
  be a real, persistent, effective-user-owned directory with mode `0700`.
  Symlinks, replacement during validation, and wrong ownership/mode fail
  closed.

The native certificate, MagicDNS name, and HTTPS enablement belong to the real
Tailnet control plane. The deterministic unit test that forces `ListenTLS` to
fail proves wrapper cleanup only; it is not issuance, hostname, or handshake
evidence.

### Separate HTTP surfaces

The loopback router on `service.listen_loopback` retains the legacy vocabulary
and operator API and the complete study surface: create/resume, GET, and sync.
Its `study.v1` namespace requires the dedicated loopback operator bearer even
when legacy routes are configured without bearer authentication.

The Tailnet router is constructed without a vocabulary `Store` and exposes
exactly:

- `POST /api/v1/study/sessions`; and
- `POST /api/v1/study/sessions/{session_id}/sync`.

Study GET, vocabulary routes, operator routes, `/healthz`, and `/version` are
not registered on the Tailnet handler. Unknown and wrong-method requests use
the bounded study error schema. Each request performs a fresh LocalAPI `WhoIs`
call with the exact native connection `RemoteAddr` before route selection or
body decoding. Forwarded identity, tag, capability, and proxy headers are
ignored. Any `Authorization` value is stripped before a successful request is
dispatched to the study service.

### Common local authorization ceiling

Both Tailnet authentication modes may authorize only `create_or_resume` and
`sync`, and only for exact profile references present in
`allowed_profile_refs`. Lexinet enforces that ceiling independently of Tailnet
policy. `get` is never part of the Tailnet ceiling.

The two modes are mutually exclusive startup configurations. There is no
request-time fallback between them.

### Tailnet-node capability mode

`tailnet_node` derives authentication and identity from native `WhoIs`:

1. The peer node must be current and have exactly one tag, byte-for-byte equal
   to `required_node_tag`.
2. The connection must carry the configured app capability. Its name is
   `<operator-controlled-domain>/cap/lexinet-study-v1`; Tailscale-reserved
   domains and domains the operator does not control are not acceptable
   deployment choices.
3. Every capability value is bounded unique-member I-JSON containing exactly
   `operations` and `profile_refs`. Arrays are nonempty, bounded,
   duplicate-free, and use the frozen study vocabularies.
4. Each capability object is intersected independently with the local ceiling.
   Separate objects remain rectangular grants; their operation and profile
   sets are never unioned into a new cross-product.
5. The request must not carry bearer authorization.

Only an opaque derived principal persists. Stable node ID bytes are mapped in
two frozen UUIDv5 stages:

1. `UUIDv5(URL, "urn:lexinet:study-principal:tailnet-node:v1")`; then
2. `UUIDv5(stage_1, "tailscale.com/control/stable-node-id/v1\x00" + exact_stable_id_bytes)`.

The resulting principal is `tailnet-node:<stage_2_uuid>`. Raw stable ID, node
name, address, user, tags, and capability documents do not become stored
identity or error context.

The following policy is a shape only. Every tag and profile containing
`replace`, and the reserved `.example` domain, must be replaced with approved
deployment values. The caller node must be enrolled with exactly the singleton
source tag:

```jsonc
{
  "tagOwners": {
    "tag:replace-with-panel-study-tag": ["autogroup:admin"],
    "tag:replace-with-lexinet-study-tag": ["autogroup:admin"]
  },
  "grants": [
    {
      "src": ["tag:replace-with-panel-study-tag"],
      "dst": ["tag:replace-with-lexinet-study-tag"],
      "ip": ["tcp:443"],
      "app": {
        "operator-controlled.example/cap/lexinet-study-v1": [
          {
            "operations": ["create_or_resume", "sync"],
            "profile_refs": ["replace.with.profile"]
          }
        ]
      }
    }
  ]
}
```

The corresponding non-secret Lexinet policy is:

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

### Scoped-token mode

`scoped_token` is a migration/compatibility alternative with narrower
credential management, not an identity-header mode:

1. Fresh native `WhoIs` must still establish a current Tailnet peer and valid
   stable node ID. Tags and app capabilities are ignored.
2. The request must carry exactly one `Bearer <token>` value matching the
   startup credential digest in constant time.
3. The token file path is absolute. The file must be regular, not a symlink,
   owned by the effective user, and mode `0400` or `0600`. Its content is 32–256
   printable non-whitespace ASCII bytes with at most one trailing LF.
4. Startup reads the file exactly once before migrations, wipes the read buffer,
   and retains only SHA-256.
5. A separately provisioned, non-Nil subject UUID supplies persistent identity.
   It is nonsecret and never derived from the token or the `WhoIs` identity.

Omit all node-mode fields:

```toml
[service]
tailnet_enabled = true
tailnet_hostname = "lexinet"
tailnet_state_dir = "/var/lib/lexinet/tsnet-state"

[service.tailnet_study_authentication]
mode = "scoped_token"
allowed_profile_refs = ["replace.with.profile"]
scoped_token_subject_id = "11111111-1111-1111-1111-111111111111" # replace
scoped_token_file = "/run/secrets/lexinet-study-token"
```

The token file is provisioned through an owner-only secret mount. Token bytes
never enter TOML, Git, command arguments, URLs, environment variables, logs,
diagnostics, HTTP errors, or image overlays.

### Enrollment inputs and logging

The running service accepts persisted, pre-enrolled identity state only. It
rejects ambient Tailscale debug-environment, auth-key, OAuth/workload-identity,
ID-token/audience, alternate-control-URL, and forced-login inputs. Both embedded
Tailscale logger callbacks are discarded, process-wide logtail buffering is
disabled, and support-log upload is disabled. Structured errors carry only
fixed public codes/messages and bounded non-secret context.

### Revocation

Revocation is explicit and tested per mode:

- For `tailnet_node`, remove the app grant or source tag, wait for control-plane
  propagation, and verify the next request is denied. A fresh `WhoIs` on every
  request means Lexinet does not cache prior tag or app-capability authority.
- For `scoped_token`, first stop or network-isolate Lexinet. Atomically install
  a new owner-only token file, choose a new subject UUID if a new persistent
  principal is intended, and restart. Removing or replacing the file while the
  process continues does not revoke the digest already retained in memory.
- If the Lexinet embedded-node state may be compromised, revoke that node in
  Tailnet administration and provision a new pre-enrolled state directory.

No mode silently degrades to the other or to unauthenticated success.

## External evidence gates

This decision fixes implementation boundaries but does not mark the deployment
step complete. Release requires sanitized, on-real-infrastructure evidence for:

1. the intended tagged panel node successfully creating and syncing only an
   allowed profile;
2. an untagged node being denied;
3. a wrong-tag or extra-tag node being denied;
4. grant/tag revocation denying the next request after propagation;
5. native certificate issuance, expected MagicDNS hostname validation, and a
   successful TLS handshake, plus fail-closed invalid-certificate behavior; and
6. destructive and cross-profile operations remaining denied.

The house-panel deployment also requires an explicit packaging choice between
an official SHA-256-pinned ARM static Tailscale archive and a pinned
source-toolchain build. Neither option is selected here. Unit, race, vet, and
synthetic certificate-failure tests are necessary local evidence but do not
replace this matrix.

## Consequences

- A compromised or misconfigured panel cannot use the Tailnet listener as a
  general Lexinet client; the route table and service interface omit those
  capabilities.
- Authorization follows the native peer connection and is re-evaluated on
  every request. Header spoofing and raw proxying do not create identity.
- Lexinet retains an independent local operation/profile ceiling even when a
  Tailnet grant is accidentally broad.
- Operators must provision identity state, MagicDNS/HTTPS, policy, revocation
  evidence, and panel packaging outside the repository before enabling the
  listener.
- Loopback administration remains available under its existing operator
  policy, while Tailnet callers never acquire operator authority.
