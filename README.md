# lexinet

lexinet is an idea-centric technical lexicon network. The root object is an idea, not a word: canonical names, acronyms, aliases, deprecated names, and localized names all resolve to one concept. Definitions, examples, notes, typed relationships, and search attach to that idea.

The project ships as one Go 1.23.4 binary named `lexinet`:

- `lexinet serve` runs the chi HTTP service on `127.0.0.1:17892` by default.
- `lexinet migrate` applies the embedded Postgres migrations.
- All other commands are JSON-first client commands for agents and operators.

## Features

- Idea resolution by UUID, UUID prefix, slug, or exact case-insensitive name.
- Name management for canonical, acronym, alias, deprecated, and localized names.
- Child bodies for definitions, examples, and notes.
- Typed relationship graph with forward/reverse labels and bounded traversal.
- Exact, prefix, full-text, and opt-in trigram fuzzy search.
- Agent-friendly CLI with `--json`, typed exit codes, stdin body support, and non-interactive destructive guards.
- Optional bearer auth for service routes other than `/healthz` and `/version`.

## Build

```sh
go build ./cmd/lexinet
```

The sandbox uses workspace-backed Go caches. If the linker runs out of tmpfs space, use:

```sh
mkdir -p .gotmp
GOTMPDIR=/workspace/.gotmp go build ./cmd/lexinet
```

## Configure

The config file defaults to `os.UserConfigDir()/lexinet/config.toml` and can be overridden with `LEXINET_CONFIG`.

Endpoint/token precedence for client commands is flag, then environment, then config, then built-in default. Token files intentionally take precedence over `LEXINET_TOKEN`.

Useful keys:

```toml
[server]
url = "http://127.0.0.1:17892"
token_file = "~/.config/lexinet/token"

[service]
listen_loopback = "127.0.0.1:17892"
auth_token = ""
tailnet_enabled = false

[service.database]
dsn = "postgres://lexinet@127.0.0.1:5432/lexinet?sslmode=disable"
```

## Run Locally

The host database and role are both `lexinet`. Required extensions are `pgcrypto`, `citext`, and `pg_trgm`.

```sh
lexinet migrate
lexinet serve --tailnet off
```

Health and version are public:

```sh
lexinet ping --json
curl http://127.0.0.1:17892/version
```

## CLI Examples

```sh
lexinet idea add "Redis" --slug redis --summary "In-memory data structure store" --json
lexinet name add redis RESP --kind acronym --expansion "Redis Serialization Protocol" --json
lexinet def add redis "Redis is an in-memory data structure server." --primary --json
lexinet search redis --json
lexinet traverse redis --direction both --depth 2 --json
```

Destructive commands such as removal and merge require `--yes` when stdin is not a TTY.

## Docker Deploy

Deployment artifacts live under `deploy/docker`:

```sh
docker compose -f deploy/docker/docker-compose.yml up --build
```

The Compose stack builds the multi-stage Dockerfile, starts Postgres, mounts `deploy/docker/config.toml`, exposes `17892`, and keeps tailnet disabled with `--tailnet off`. See `deploy/docker/README.md` for auth, database, and tailnet enablement notes.

## Architecture

The Mission Control architecture graph is persisted as `lexinet-current-architecture`. Source specs are kept in `docs/architecture/`:

- `docs/architecture/lexinet-architecture.json`
- `docs/architecture/lexinet-flows.json`

The graph records the agent CLI, HTTP service on `:17892`, Postgres, the idea/name/body model, relationship graph, and search index. Data-flow edges were authored with `mission-control architecture flow --set-dataflow`.

## Validation

Final Phase 8 sandbox validation evidence is in `docs/validation/phase-8.md`.

Database integration remains host/operator verified because this sandbox must not reach host Postgres directly.
