# Phase 8 Validation Evidence

Date: 2026-05-31

## Git Slices

- `bb6a589 Add Docker deployment artifacts`
- `9d4c771 Record architecture graph spec`

This file and the top-level README are committed in the final handoff slice.

## Sandbox Verification

All commands ran from `/workspace` with `GOTMPDIR=/workspace/.gotmp`.

```sh
GOTMPDIR=/workspace/.gotmp go build ./cmd/lexinet
GOTMPDIR=/workspace/.gotmp go vet ./...
GOTMPDIR=/workspace/.gotmp go test ./...
```

Result: pass.

`go test ./...` package summary:

```text
?    lexinet/cmd/lexinet       [no test files]
?    lexinet/internal/config   [no test files]
ok   lexinet/internal/cli      (cached)
ok   lexinet/internal/model    (cached)
ok   lexinet/internal/server   (cached)
?    lexinet/internal/tailnet  [no test files]
?    lexinet/migrations        [no test files]
ok   lexinet/internal/storage  (cached)
```

## Mission Control Architecture

Contract loaded first:

```sh
mission-control architecture contract --json
```

Graph writes used the HTTP-backed architecture CLI, with no `--db` flag:

```sh
mission-control architecture upsert \
  --project a927c12b-ca9e-4cc5-a492-0b5acb09d2e8 \
  --file docs/architecture/lexinet-architecture.json \
  --allow-creates \
  --json

mission-control architecture flow \
  --project a927c12b-ca9e-4cc5-a492-0b5acb09d2e8 \
  --slug lexinet-current-architecture \
  --file docs/architecture/lexinet-flows.json \
  --set-dataflow \
  --json
```

Verification:

```sh
mission-control architecture verify \
  --project a927c12b-ca9e-4cc5-a492-0b5acb09d2e8 \
  --slug lexinet-current-architecture \
  --full \
  --json
```

Result: `ok=true`, `graphKind=dataflow`, 6 nodes, 8 edges, 0 warnings.

## Deploy Artifacts

- `Dockerfile`: multi-stage `golang:1.23.4-bookworm` build to a distroless minimal runtime.
- `deploy/docker/docker-compose.yml`: lexinet service plus Postgres 16 with health checks.
- `deploy/docker/config.toml`: container service config for `0.0.0.0:17892` and Compose Postgres.
- `deploy/docker/README.md`: launch, auth, validation, and tailnet enablement notes.

The Compose stack was not started in the sandbox because Phase 8 only required artifact authoring plus Go build/vet/unit validation, and live service/database startup remains operator-controlled.

## Host DB Checklist

Host-side checks are operator-owned because the sandbox must not connect to host Postgres. The final operator checklist is:

- Embedded migrations apply cleanly.
- Idea creation returns one idea with canonical name.
- Alias/acronym resolution returns the same idea with all names.
- Definitions attach and are searchable.
- Relationship list and traversal return expected typed edges.
- Full-text and fuzzy search both return expected idea results.
- Unknown JSON fields are rejected with `400 BAD_REQUEST`.
