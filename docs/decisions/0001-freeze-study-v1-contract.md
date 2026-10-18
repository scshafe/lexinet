<!-- @canonical study-v1-contract-decision -->

# Decision 0001: Freeze the `study.v1` Cross-Repository Contract

**Status:** accepted

**Date:** 2026-08-16

## Context

The house panel needs resumable study sessions, immutable card projections,
append-only rating evidence, bounded offline synchronization, and stable replay
semantics. Lexinet remains authoritative for vocabulary content, session order,
event adjudication, and scheduler provenance. A prose-only agreement would let
the server and panel drift independently.

Lexinet's existing API returns raw success resources and errors shaped as
`{"error":{"code":"...","message":"..."}}`. Changing every endpoint to a
new envelope is unrelated to the study capability and is not authorized here.

## Decision

Freeze `study.v1` in
[`contracts/study.v1/openapi.json`](../../contracts/study.v1/openapi.json) as
the sole machine-readable authority. It exposes exactly these operations:

- `POST /api/v1/study/sessions` creates or resumes the one nonterminal session
  for the normalized learner/corpus/card-kind scope;
- `GET /api/v1/study/sessions/{session_id}` resumes an issued session and
  returns a byte-stable bounded page; and
- `POST /api/v1/study/sessions/{session_id}/sync` atomically adjudicates a
  contiguous client-stream batch and returns terminal receipts and a bounded
  issued page.

Every success resource identifies `protocol_version = study.v1`,
`schema_version = 1`, and the exact ordered required-semantic set. Requests are
strict. Responses may gain optional fields, but a new required field, new
required semantic, or changed meaning requires a new protocol version and a
coordinated consumer update.

### Session authority

The create request carries a client-generated `creation_request_id`, explicit
profile reference, normalized sorted unique topic set, card kind, bounded page
request, and an optional predecessor plus flush-receipt pair. The creation UUID
is bound to a normalized request SHA-256. An identical replay returns the same
receipt; changed normalized input returns `IDEMPOTENCY_CONFLICT`.

Distinct concurrent creation UUIDs for one active scope converge on the same
authoritative unfinished session. The server, not a client's local page, owns
cycle exhaustion. A successor requires a receipt proving that the predecessor
stream is flushed and a canonical `presented` fact for every materialized
predecessor item. One locked transaction validates the barrier, records the
input ingestion high-water mark, terminalizes the predecessor, and links one
successor. An unchanged empty corpus converges on one
`waiting_for_corpus` sentinel until a changed eligible corpus atomically
supersedes it.

### Projection and scheduler authority

Every issued item is an immutable complete front/back/example projection bound
to stable card/source identities, a source revision, and a content SHA-256.
Every issued cycle carries scheduler name/version, configuration digest,
planning time, seed, corpus digest, input per-profile ingestion high-water
ordinal, and per-item selection provenance. Clients display those resources;
they never reproduce server selection.

### Event and receipt authority

Sync accepts only ordered `presented`, `revealed`, `rated`, and
`rating_voided` events under one durable `client_stream_id` and contiguous
positive sequence. `presented` and `revealed` are singular facts. A rating is
exactly `again`, `hard`, `good`, or `easy`; a void names the exact effective
rating UUID. Causal prerequisites must exist durably or precede the dependent
event in the same atomic batch.

Every terminally adjudicated sequence receives an immutable receipt with its
event UUID, normalized payload SHA-256, disposition, stable code, optional
canonical event UUID, and optional per-profile ingestion ordinal. Accepted
events alone become scheduler facts. Semantic duplicates and stable terminal
rejections consume sequence positions; retryable request failures do not.
Completed predecessors continue to accept valid retained rating/correction
evidence, but late facts cannot rewrite an already materialized successor.

### Bounds and envelopes

The contract caps request bodies at 262,144 bytes and response bodies at
1,048,576 bytes; topic sets at 32; event batches at 64; and returned pages at
64 items. Identifier/text bounds live beside their schemas. These are protocol
ceilings, not target cache defaults.

Study successes remain raw resources. Study errors retain Lexinet's existing
top-level `error` envelope. Stable HTTP-level codes and stable per-event receipt
codes are enumerated in the canonical contract. Valid empty corpus is a
`waiting_for_corpus` success; malformed, unauthorized, stale, oversized, and
unavailable data are explicit failures, never plausible empty success.

## Consequences

- Lexinet owns one contract and one validator; consumers export
  provenance-stamped examples rather than author another schema.
- Later persistence, scheduler, handler, CLI, authorization, and panel-client
  work can implement against a fixed semantic surface.
- Optional response evolution remains possible inside `study.v1`, while
  incompatible change fails visibly at the consumer boundary.
- The contract does not implement any study route, authorize the panel, or
  claim production readiness.
