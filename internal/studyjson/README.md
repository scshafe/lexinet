# Study JSON Canonicalization

This leaf package owns I-JSON preflight and RFC 8785 transformation for
normalized study request identity documents. Preflight rejects duplicate
members, malformed surrogate pairs,
Unicode noncharacters, non-finite numbers, and plain integers outside the
interoperable IEEE-754 range before the canonicalizer can coerce them. A 1 MiB
document cap and 64-container nesting cap bound both preflight and transform
work. The package has no model, storage, or service dependencies.

`CanonicalizeUnique` is the `@canonical study_json_jcs` seam. Request-identity
callers wrap its errors in their own bounded domain errors and never log the
rejected JSON bytes. Persisted SessionPage artifacts and complete responses do
not use this package; they retain full int64 values through strict typed
`encoding/json`.
