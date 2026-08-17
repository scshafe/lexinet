# Tailnet study authentication

This package owns Lexinet's single Tailnet-to-study trust boundary. It validates
one immutable startup policy, authenticates every Tailnet request with a fresh
local-client `WhoIs`, derives a non-secret study principal, and issues private
context claims consumed by study authorization. It does not open listeners,
route HTTP, read forwarded identity headers, or authorize vocabulary routes.

## Canonical boundary

`Boundary` is the `@canonical tailnet_study_authentication` implementation. A
server constructs it once with `NewBoundary`, calls `Authenticate` with the
native connection's exact remote address and all `Authorization` values, then
passes the returned context and principal to the shared `StudySessionService`.
`PrincipalFromContext` accepts claims only from that exact boundary instance.
`AuthorizeStudy` preserves the established loopback operator policy, while
every non-operator principal needs issuer-bound Tailnet claims.

Authentication has exactly two mutually exclusive startup modes:

- `tailnet_node` requires one exact configured Tailscale tag and an application
  capability named `<operator-controlled-domain>/cap/lexinet-study-v1`. Every
  capability value is unique-member I-JSON with exactly `operations` and
  `profile_refs`. There may be at most 16 values of 8,192 bytes each; every
  operation/profile array is nonempty, duplicate-free, bounded, and syntactically
  valid before it is intersected with Lexinet's configured create/resume-and-
  sync/profile ceiling. Each nonempty intersected object remains one rectangular
  grant, so unions cannot create an unauthorized operation/profile cross-product.
  The caller must send no bearer header.
- `scoped_token` still requires successful fresh `WhoIs`, but ignores node tags
  and application capabilities. It requires exactly one `Bearer <token>` value
  matching an absolute, regular, effective-user-owned `0400` or `0600` file.
  The file contains 32–256 printable non-whitespace ASCII bytes and may end in
  one LF. Startup retains only SHA-256. Identity comes from a separately
  provisioned subject UUID, never token bytes.

Node mode requires the `WhoIs` node to have exactly the configured singleton
tag. Its opaque stable node ID is accepted only at 1–256 bytes and is mapped in
two frozen UUIDv5 stages:

1. `UUIDv5(URL, "urn:lexinet:study-principal:tailnet-node:v1")`
2. `UUIDv5(stage_1, "tailscale.com/control/stable-node-id/v1\x00" + exact_id_bytes)`

Only `tailnet-node:<stage_2_uuid>` persists. The raw stable ID, node name,
address, user, tag, capability JSON, and token never become identity or error
context. No trimming, case folding, or format parsing changes stable ID bytes.

## Trust and failure model

The protected asset is authority to mutate learner study state. Realistic
threats are a nontagged Tailnet peer, a peer with a broader or malformed grant,
a spoofed HTTP identity/capability header, a bearer exposed through process
arguments or logs, and one boundary instance replaying another's context. The
controls are fresh transport-derived identity, exact tag cardinality, closed
bounded app-cap parsing, a local authorization ceiling, owner-only token files,
constant-time digest comparison, and private issuer-bound claims.

The package deliberately does not claim to prove DNS-domain ownership, protect
against a compromised Tailscale control plane, protect token bytes after an HTTP
stack has received the header, or replace native certificate validation. The
operator must configure a domain they control, the listener must use native
`tsnet.ListenTLS`, and external deployment/revocation tests remain required.

Expected failures are structured `model.StudyError` values with fixed messages
and fixed non-secret context. External WhoIs and filesystem causes, supplied
paths, addresses, stable IDs, tags, capability documents, and credentials are
not chained or copied into errors. There is no request-time fallback between
modes and no degraded success.

## Dependencies

The package depends on Tailscale's stable `WhoIs` response and peer-capability
types, `internal/studyjson` for unique-member I-JSON canonicalization,
`internal/model` for opaque principals and structured errors, and
`internal/studysession` for the canonical operation/profile validators and
`ScopedStudyAuthorizer`. Listener lifecycle and HTTP route separation remain in
their owning domains.
