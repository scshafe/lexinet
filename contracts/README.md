# Machine-readable contracts

This directory owns Lexinet's canonical externally consumed API contracts.
Each versioned contract has one directory containing its OpenAPI/JSON Schema
authority, compatibility rules, golden examples, and validation tests.

- [`study.v1`](study.v1/README.md) freezes the first panel study protocol.

Consumers may export content-identified fixtures from these contracts. They
must not maintain a second hand-authored schema.
