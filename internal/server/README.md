<!-- @canonical lexinet_http_server_boundary -->

# HTTP server boundary

This domain owns Lexinet's inbound HTTP trust boundary and host-server budgets.
`Router` preserves the legacy API surface while `WithStudySessionService` mounts
the separate `study.v1` namespace. `NewHTTPServer` is the only authority for
read-header, read/body, write, idle, and header-size limits, and `Serve` uses it
for both loopback and Tailnet listeners.

Study routes deliberately do not reuse legacy optional bearer authentication or
the permissive legacy JSON decoder. Their middleware generates correlation
identity, sanitizes panics, requires one exact dedicated bearer even when the
configured token is empty, and injects only `OperatorStudyPrincipalID`. The
strict transport codec enforces media type, encoding, raw-byte, UTF-8, nesting,
duplicate-member, exact-member, and single-value bounds before a handler calls
exactly one method on the focused `StudySessionService` interface. HTTP errors
are reconstructed from `studysession.PublicStudyError`; raw messages, contexts,
causes, request bodies, credentials, and projection content never cross the
boundary.

The domain depends on `internal/studysession` for protocol types and canonical
composition, `internal/model` for opaque principals and stable error codes,
`internal/storage` for the production repository, and `internal/study` for the
active scheduler selected only during `Serve` composition.
