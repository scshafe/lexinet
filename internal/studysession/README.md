# Study session service

This package is the public composition boundary for the frozen `study.v1`
protocol. It owns create-or-resume, get, and sync orchestration. HTTP handlers
and operator clients exchange the strict wire DTOs in this package; they do not
coordinate storage calls themselves.

## Canonical boundaries

- `StudySessionService` is the `@canonical study_session_service`. Every study
  operation crosses its authorization, non-queuing admission, normalization,
  repository, committed-readback, paging, and response-size checks.
- `CanonicalizeStudyRequestJSON` is the `@canonical study_request_jcs` service
  entry point. It delegates unique-member I-JSON enforcement and RFC 8785
  transformation to the shared `internal/studyjson` leaf. Request identities
  hash domain-normalized bytes, never raw request bytes or ordinary
  `encoding/json` output.
- `OperatorStudyAuthorizer` is the explicit Step 4 transition policy. It
  grants create, read, and sync only to the stable operator principal. It is an
  authorization policy, not a missing check; Step 5 may replace the policy and
  principal derivation without changing the service or repository interfaces.
- `StudyAuthoritySource` is the only clock, cryptographic seed, session UUID,
  and session-item UUID authority. The cycle materializer invokes its item-ID
  allocator only after receipt preflight and locked nonempty corpus capture.

## Trust and failure model

Capability authorization runs inside the operation deadline before admission
or repository access. GET and sync then use a narrow profile lookup and a
second resolved-target authorization before reading any item or page content.
Admission queues no work: at most four authoritative calls globally and two
for one principal may run concurrently. Expected failures return allowlisted
`model.StudyError` codes with fixed safe messages; repository causes, internal
context, request content, profile references, seeds, and identifiers are not
copied into the error surface. Protocol adapters render failures only through
`PublicStudyError`, the closed code/message/retryability descriptor.

Creation validates one fresh consistent committed read containing the exact
receipt, immutable issuance fields, and ordered items. Optional shadow work is
post-commit and observation-only. It runs only for a newly created, active,
nonempty cycle after that readback comparison; replay, convergence, waiting,
readback mismatch, and every shadow outcome leave authoritative results
unchanged.

Sync binds the requested page to the durable flush receipt as exact,
deterministic typed `encoding/json` bytes and a SHA-256. The same typed page
encoder is used when that page is nested in a response, preserving the full
`int64` domain without floating-point canonicalization. Replays consume the
stored page artifact instead of rebuilding it from mutable live-source links.
Pages contain at most 64 items and at most 983,040 encoded bytes; complete
responses use ordinary typed `encoding/json` and are checked against the 1 MiB
protocol ceiling before a caller can write headers.

## Dependencies

The package depends on `internal/model` for durable domain values,
`internal/study` for canonical materialization and isolated shadow evaluation,
and a focused `StudySessionRepository` adapter for persistence. The storage
package never imports this package.
