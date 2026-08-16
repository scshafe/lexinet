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
- Durable capture submissions for later agent enrichment.
- Child bodies for definitions, examples, and notes.
- Typed relationship graph with forward/reverse labels and bounded traversal.
- Configurable exact, prefix, full-text, and trigram fuzzy search strategies.
- Optional native macOS menu-bar lookup helper in `clients/macos`.
- Agent-friendly CLI with `--json`, typed exit codes, stdin body support, and non-interactive destructive guards.
- Optional bearer auth for loopback-only service routes, and mandatory bearer auth whenever Lexinet's native Tailnet listener is enabled; `/healthz` and `/version` remain public.

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

The built-in service default is loopback-only with `tailnet_enabled = false`, so local development may leave `service.auth_token` empty. Enabling Lexinet's native Tailnet listener requires a nonempty, non-whitespace `service.auth_token`; an unsafe effective configuration fails before migrations, database access, or listener startup. This validation cannot detect an external TCP proxy aimed at the loopback listener; configure the bearer before exposing Lexinet through `tailscale serve --tcp` or any other proxy.

### Search Strategy Configuration

Search runs through a named server-side strategy. The built-in default is `daily-driver`, which enables exact, prefix, full-text, fuzzy name, and fuzzy body matchers so menu-bar searches get typo tolerance without a client rebuild. `legacy` preserves the older default where fuzzy matchers are disabled unless the request opts in.

Config shape:

```toml
[search]
default_limit = 20
default_strategy = "daily-driver"

[search.strategies.daily-driver]
fuzzy = true
threshold = 0.3

[[search.strategies.daily-driver.matchers]]
name = "exact"
enabled = true
weight = 1.0
priority = 0

[[search.strategies.daily-driver.matchers]]
name = "prefix"
enabled = true
weight = 0.8
priority = 1

[[search.strategies.daily-driver.matchers]]
name = "full-text"
enabled = true
weight = 0.6
priority = 2

[[search.strategies.daily-driver.matchers]]
name = "fuzzy-name"
enabled = true
weight = 0.5
threshold = 0.3
priority = 3

[[search.strategies.daily-driver.matchers]]
name = "fuzzy-body"
enabled = true
weight = 0.3
threshold = 0.3
priority = 4
```

Matcher names are `exact`, `prefix`, `full-text`, `fuzzy-name`, and `fuzzy-body`. Lower `priority` sorts ahead when scores tie closely enough, so exact and prefix results rank ahead of fuzzy candidates in the shipped strategy. Partial strategy definitions are merged with the built-ins: omitted built-in matchers keep their default weight, threshold, and priority.

Compatibility mapping:

- `GET /search?q=...` and `lexinet search <q>` use `search.default_strategy`, which defaults to `daily-driver`.
- `strategy=legacy` or `lexinet search <q> --strategy legacy` selects the legacy config with fuzzy disabled.
- Existing `fuzzy=true|false`, `--fuzzy`, and `--no-fuzzy` are per-request overrides of the selected strategy's fuzzy behavior.
- Existing `threshold`, `--threshold`, and `--min-score` are per-request overrides for the selected strategy's fuzzy thresholds.
- Existing `scope`, `kind`, `limit`, and `offset` keep their previous request semantics.
- Responses include top-level `strategy` plus `provenance` for the effective strategy and per-result `matcher`/`provenance`, so HTTP and CLI callers can see which matcher produced each hit.

Config reload behavior: `lexinet serve` loads the config when the process starts. Changing `search.default_strategy` or any `search.strategies.*` value requires restarting `lexinet serve`; there is no hot reload watcher. Clients do not need rebuilding because the menu-bar app sends no strategy/fuzzy/threshold parameters by default and therefore follows the restarted server's default strategy.

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

## Capture Submissions

Use submissions when you want to capture a term quickly and let an enrichment agent create or link the final idea later. A submission starts as `pending`, then an agent or operator marks it `enriched` with an idea reference or `failed` with a note.

CLI workflow:

```sh
lexinet submit "TUI" --context "Saw this in a terminal UI note" --json
lexinet submission list --status pending --limit 20 --json
lexinet submission list --status pending --jsonl

lexinet idea add "Terminal User Interface" --slug terminal-user-interface --summary "Interactive text UI for terminal sessions" --json
lexinet submission complete 77777777-7777-7777-7777-777777777777 terminal-user-interface --note "Created expansion idea" --json
lexinet submission fail 88888888-8888-8888-8888-888888888888 --note "Duplicate or not in scope" --json
```

HTTP workflow:

```sh
curl -sS -X POST http://127.0.0.1:17892/submissions \
  -H 'content-type: application/json' \
  -d '{"term":"TUI","context":"Saw this in a terminal UI note"}'

curl -sS 'http://127.0.0.1:17892/submissions?status=pending&limit=20&offset=0'

curl -sS -X POST http://127.0.0.1:17892/submissions/77777777-7777-7777-7777-777777777777/complete \
  -H 'content-type: application/json' \
  -d '{"idea":"terminal-user-interface","note":"Created expansion idea"}'

curl -sS -X POST http://127.0.0.1:17892/submissions/88888888-8888-8888-8888-888888888888/fail \
  -H 'content-type: application/json' \
  -d '{"note":"Duplicate or not in scope"}'
```

The list endpoint accepts `status=pending|enriched|failed`, `limit`, and `offset`. Completion resolves `idea` with the same idea-reference semantics used elsewhere in the API and rejects terminal submissions instead of mutating them.

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

Click the `Lexinet` menu-bar item, search a term, and confirm results show names, definition text, topics, and acronym-link summaries when present. The app shows a service-unavailable hint instead of exiting when the Go service is down. The `+` button opens capture mode, where `Submit to Agent` queues a pending submission and the disabled direct `Submit` button remains a visible placeholder. See `clients/macos/README.md` for local install, login-item setup, and manual capture validation.

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

Daily-driver manual acceptance for topics, acronym-entry links, TUI/Terminal User Interface linking, capture submissions, and macOS menu-bar setup is in `docs/validation/v2-daily-driver.md`.

Database integration remains host/operator verified because this sandbox must not reach host Postgres directly.
