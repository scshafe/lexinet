<!-- @canonical lexinet_http_server_boundary -->

# HTTP server boundary

This domain owns Lexinet's inbound HTTP trust boundary and host-server budgets.
`Router` preserves the legacy API surface while `WithStudySessionService` mounts
the separate `study.v1` namespace. `NewHTTPServer` is the only authority for
read-header, read/body, write, idle, and header-size limits. `ServePrepared`
uses those budgets for two intentionally different HTTP surfaces: the ordinary
loopback router and, when enabled, a native-TLS Tailnet study router on fixed
port 443. They never share a handler.

Loopback study routes deliberately do not reuse legacy optional bearer
authentication or the permissive legacy JSON decoder. Their middleware
generates correlation identity, sanitizes panics, requires one exact dedicated
bearer, strips it before dispatch, and injects only `OperatorStudyPrincipalID`.
The loopback router retains create/resume, GET, and sync plus the operator and
vocabulary APIs.

`NewTailnetStudyRouter` is the separate appliance boundary. Its constructor has
no vocabulary `Store` capability, and its route table contains only
`POST /api/v1/study/sessions` and
`POST /api/v1/study/sessions/{session_id}/sync`. It authenticates every request
with a fresh LocalAPI `WhoIs` call against the exact native connection
`RemoteAddr` before routing or body decoding. It does not expose GET,
`/healthz`, `/version`, operator routes, or vocabulary routes and does not trust
forwarded identity, capability, or proxy headers.

Both study surfaces use the strict transport codec, which enforces media type,
encoding, raw-byte, UTF-8, nesting, duplicate-member, exact-member, and
single-value bounds before a handler calls exactly one method on its focused
service interface. HTTP errors are reconstructed from
`studysession.PublicStudyError`; raw messages, contexts, causes, request bodies,
credentials, and projection content never cross the boundary.

`PrepareConfiguration` is the startup validation authority. It builds the
Tailnet authentication boundary, reads a configured scoped-token file exactly
once, retains only its SHA-256 digest, and validates the native transport before
migrations or listener startup. Production passes that detached result through
migration to `ServePrepared`; it does not re-read credentials. A Tailnet or
loopback server exit shuts down both servers and joins their goroutines before
the native endpoint is closed.

The domain depends on `internal/studysession` for protocol types and canonical
composition, `internal/model` for opaque principals and stable error codes,
`internal/storage` for the production repository, and `internal/study` for the
active scheduler selected only during serve composition. It depends on
`internal/studyauth` for per-request Tailnet identity and scoped authorization,
and on `internal/tailnet` for the native TLS listener lifecycle.
