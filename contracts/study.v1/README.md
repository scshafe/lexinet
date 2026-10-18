<!-- @canonical study-v1-contract -->

# `study.v1` contract

[`openapi.json`](openapi.json) is the canonical OpenAPI 3.1 and JSON Schema
authority for the study protocol. [`manifest.json`](manifest.json) identifies
the golden valid and intentionally invalid examples exercised by the contract
validator.

The contract freezes request/response shapes and semantics; it does not claim
that the study handlers, persistence model, scheduler, or scoped Tailnet
authorization already exist. Those implementation gates remain separate.

Compatibility is asymmetric:

- a server may add optional response fields within `study.v1`;
- requests remain strict and reject unknown fields;
- a semantic change, new required field, new required semantic, or changed
  meaning requires a new protocol version and coordinated consumer update.

Success responses are raw typed resources. Errors retain Lexinet's
`{"error":{"code":"...","message":"..."}}` envelope, with the study
protocol and schema versions carried inside the error object.

## Atomic sync clarification

A sync window is reconciled as an immutable replay prefix followed by at most
one new contiguous suffix. Either partition may be empty. Every replayed event
must retain its original event identity, normalized payload digest, and
terminal receipt.

A valid new suffix may mix `accepted` and `semantic_duplicate` receipts. If
any new event instead encounters a terminal domain failure, the whole new
suffix is terminally rejected: every new-suffix receipt uses
`terminal_rejected` and the first failure code, and no new-suffix canonical
event or per-profile ingestion ordinal is committed. The already-adjudicated
replay prefix remains unchanged. Retryable request failures commit no receipt
or sequence position.

An identical `sync_id` replay preserves the original event receipts, flush
receipt, and immutable issued page bytes. The surrounding session resource is
live and may reflect a later authoritative lifecycle or state. Consequently,
`study.v1` does not promise byte stability for the whole sync response.

The executable examples name their replay boundary in `manifest.json`.
Decision [0002](../../docs/decisions/0002-clarify-study-v1-atomic-sync.md)
records why this narrows an ambiguity in the accepted atomic meaning without
changing the wire shape, required semantics, schema version, or protocol
version.

Run the focused validator with:

```sh
go test ./contracts/study.v1
```
