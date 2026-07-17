# lexinet

lexinet is an idea-centric technical lexicon network. The root object is an idea, not a word: canonical names, acronyms, aliases, deprecated names, and localized names all resolve to one concept. Definitions, examples, notes, typed relationships, and search attach to that idea.

The project ships as one Go 1.23.4 binary named `lexinet`:

- `lexinet serve` runs the chi HTTP service on `127.0.0.1:17892` by default.
- `lexinet migrate` applies the embedded Postgres migrations.
- All other commands are JSON-first client commands for agents and operators.

## Features

- Idea resolution by UUID, UUID prefix, slug, or exact case-insensitive name.
- Name management for canonical, acronym, alias, deprecated, and localized names.
- First-class topics that can be created, renamed, assigned to ideas, and used to filter idea lists.
- Acronym-as-entry links that connect a standalone acronym idea to its standalone expansion idea.
- Child bodies for definitions, examples, and notes.
- Typed relationship graph with forward/reverse labels and bounded traversal.
- Exact, prefix, full-text, and opt-in trigram fuzzy search.
- Optional native macOS menu-bar lookup helper in `clients/macos`.
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

## Daily-Driver Workflows

Topics organize ideas without changing the idea-centric model. Create topics once, assign them by topic slug/name/ID, then list ideas through either the topic command or the general idea list filter:

```sh
lexinet topic add software --description "Software engineering concepts" --json
lexinet topic add literature --description "Reading, criticism, and narrative concepts" --json

lexinet idea add "Terminal User Interface" --slug terminal-user-interface --summary "Interactive text UI for terminal sessions" --json
lexinet idea add "TUI" --slug tui --summary "A standalone acronym entry for Terminal User Interface" --json
lexinet idea add "Intertextuality" --slug intertextuality --summary "Meaning shaped by relationships between texts" --json

lexinet topic assign terminal-user-interface software --json
lexinet topic assign tui software --json
lexinet topic assign intertextuality literature --json

lexinet topic ideas software --json
lexinet idea list --topic literature --json
lexinet idea get terminal-user-interface --json
```

HTTP clients use the same model:

```sh
curl -sS -X POST http://127.0.0.1:17892/topics \
  -H 'content-type: application/json' \
  -d '{"slug":"software","name":"software","description":"Software engineering concepts"}'

curl -sS -X POST http://127.0.0.1:17892/ideas/terminal-user-interface/topics \
  -H 'content-type: application/json' \
  -d '{"topic":"software"}'

curl -sS 'http://127.0.0.1:17892/topics/software/ideas?limit=20'
curl -sS 'http://127.0.0.1:17892/ideas?topic=software&limit=20'
```

Acronym names and acronym entries are deliberately different:

- An acronym name is another name on the same idea, created with `lexinet name add <idea> <text> --kind acronym --expansion ...`. Resolving either name returns one idea.
- An acronym entry is its own idea, linked to a separate expansion idea with `lexinet acronym link <acronym-idea> <expansion-idea>`. Resolving either idea surfaces the link to the other through `acronym_links`.

Use acronym entries when the acronym deserves its own definition, notes, examples, topic assignments, or relationship graph position:

```sh
lexinet acronym link tui terminal-user-interface --note "Common abbreviation" --json
lexinet acronym links tui --json
lexinet acronym links terminal-user-interface --json
lexinet name add terminal-user-interface "Terminal UI" --kind alias --json
lexinet name promote terminal-user-interface "Terminal UI" --json
lexinet acronym links tui --json
```

The HTTP equivalent is:

```sh
curl -sS -X POST http://127.0.0.1:17892/acronym-links \
  -H 'content-type: application/json' \
  -d '{"acronym":"tui","expansion":"terminal-user-interface","note":"Common abbreviation"}'

curl -sS http://127.0.0.1:17892/ideas/tui/acronym-links
curl -sS http://127.0.0.1:17892/ideas/terminal-user-interface/acronym-links
```

The normal relationship graph remains available for semantic links. For example, link the acronym entry into the expansion's definition context:

```sh
lexinet link add tui used_in_definition_of terminal-user-interface --note "Acronym appears in the expansion entry" --json
lexinet traverse tui --relation used_in_definition_of --depth 1 --json
```

## macOS Menu-Bar Lookup

The optional native menu-bar client lives in `clients/macos` and talks to `lexinet serve` at `http://127.0.0.1:17892`.

```sh
lexinet serve --tailnet off
cd clients/macos
swift build
swift test
swift run LexinetMenuBar
```

Click the `Lexinet` menu-bar item, search a term, and confirm results show names, definition text, topics, and acronym-link summaries when present. The app shows a service-unavailable hint instead of exiting when the Go service is down. See `clients/macos/README.md` for local install and login-item setup.

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

The graph records the agent CLI (`agent-cli`), HTTP service on `:17892` (`http-service`), Postgres (`postgres`), the idea/name/body model (`idea-name-body-model`), relationship graph (`relationship-graph`), search index (`search-index`), topic/acronym storage (`topic-acronym-model`), and macOS menu-bar client (`menu-bar-client`). Key v2 flows include `agent-cli-to-http-service`, `http-service-to-topic-acronym-model`, `menu-bar-client-to-http-service`, and `postgres-contains-topic-acronym-model`.

## Validation

Final Phase 8 sandbox validation evidence is in `docs/validation/phase-8.md`.

Daily-driver manual acceptance for topics, acronym-entry links, TUI/Terminal User Interface linking, and macOS menu-bar setup is in `docs/validation/v2-daily-driver.md`.

Database integration remains host/operator verified because this sandbox must not reach host Postgres directly.
