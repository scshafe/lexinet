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

Run the focused validator with:

```sh
go test ./contracts/study.v1
```
