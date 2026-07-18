# lexinet v2 daily-driver manual acceptance

This walkthrough verifies the topic, acronym-entry, TUI/Terminal User Interface, capture submission, configurable search strategy, and menu-bar workflows against a real Postgres-backed service. It is intentionally operator-run: the sandbox must not reach or mutate the host database.

## Preconditions

- Run on the host or a disposable Postgres database, not from the sandbox.
- `lexinet migrate` has been applied to that database.
- `lexinet serve --tailnet off` is running on `127.0.0.1:17892`.
- If bearer auth is enabled, export the normal `LEXINET_TOKEN_FILE` or pass the configured token flags.
- Use temporary slugs if the shared lexicon already contains these entries.

## Topic Workflow

Create daily-driver topics and sample ideas:

```sh
lexinet topic add software --description "Software engineering concepts" --json
lexinet topic add literature --description "Reading, criticism, and narrative concepts" --json

lexinet idea add "Terminal User Interface" --slug terminal-user-interface --summary "Interactive text UI for terminal sessions" --json
lexinet idea add "TUI" --slug tui --summary "A standalone acronym entry for Terminal User Interface" --json
lexinet idea add "Command-Line Interface" --slug command-line-interface --summary "Command-oriented interface for terminals" --json
lexinet idea add "Intertextuality" --slug intertextuality --summary "Meaning shaped by relationships between texts" --json
lexinet idea add "Free Indirect Discourse" --slug free-indirect-discourse --summary "Narration blending character and narrator voice" --json
```

Assign and list topics from the CLI:

```sh
lexinet topic assign terminal-user-interface software --json
lexinet topic assign tui software --json
lexinet topic assign command-line-interface software --json
lexinet topic assign intertextuality literature --json
lexinet topic assign free-indirect-discourse literature --json

lexinet topic list --json
lexinet topic ideas software --json
lexinet topic ideas literature --json
lexinet idea list --topic software --json
lexinet idea get terminal-user-interface --json
```

Expected result: topic list shows `software` and `literature`; each topic idea list contains the assigned ideas; idea detail includes a `topics` array.

Verify the same workflow over HTTP:

```sh
curl -sS http://127.0.0.1:17892/topics
curl -sS 'http://127.0.0.1:17892/topics/software/ideas?limit=20'
curl -sS 'http://127.0.0.1:17892/ideas?topic=literature&limit=20'
curl -sS http://127.0.0.1:17892/ideas/terminal-user-interface
```

Expected result: HTTP responses include the same topics and topic-filtered ideas as the CLI.

## Acronym Entry Workflow

Choose acronym-entry links when the acronym should be its own idea. Use acronym names when the acronym is only another surface form for one idea.

Create both forms deliberately:

```sh
lexinet name add terminal-user-interface TUI --kind acronym --expansion "Terminal User Interface" --json
lexinet acronym link tui terminal-user-interface --note "Acronym entry expands to the full terminal UI concept" --json
```

Expected result: resolving the acronym name on `terminal-user-interface` still returns the expansion idea, while resolving the standalone `tui` idea returns its own entry.

Check both sides of the acronym-entry pair:

```sh
lexinet acronym links tui --json
lexinet acronym links terminal-user-interface --json
curl -sS http://127.0.0.1:17892/ideas/tui/acronym-links
curl -sS http://127.0.0.1:17892/ideas/terminal-user-interface/acronym-links
```

Expected result: the `tui` response has direction `acronym`; the `terminal-user-interface` response has direction `expansion`; both responses reference the other idea name.

Verify the pair survives an expansion canonical-name change:

```sh
lexinet name add terminal-user-interface "Terminal UI" --kind alias --json
lexinet name promote terminal-user-interface "Terminal UI" --json
lexinet acronym links tui --json
```

Expected result: the acronym link still points at the expansion idea and shows the current expansion name exposed by the API.

Link TUI into the relationship graph as a normal idea as well:

```sh
lexinet link add tui used_in_definition_of terminal-user-interface --note "Acronym appears in the expansion entry" --json
lexinet traverse tui --relation used_in_definition_of --depth 1 --json
```

Expected result: traversal includes the TUI to Terminal User Interface relationship while acronym-entry links remain available through `lexinet acronym links`.

## Capture Submissions Workflow

Queue a term from the CLI:

```sh
lexinet submit "TUI" --context "Captured from menu-bar workflow" --json
lexinet submission list --status pending --limit 20 --json
```

Expected result: the submitted term appears as `status: "pending"` with its context preserved.

Verify the same workflow over HTTP:

```sh
curl -sS -X POST http://127.0.0.1:17892/submissions \
  -H 'content-type: application/json' \
  -d '{"term":"CLI","context":"Captured through curl"}'

curl -sS 'http://127.0.0.1:17892/submissions?status=pending&limit=20&offset=0'
```

Complete one pending submission after a real idea exists:

```sh
lexinet idea add "Command-Line Interface" --slug command-line-interface --summary "Command-oriented interface for terminals" --json
lexinet submission complete 77777777-7777-7777-7777-777777777777 command-line-interface --note "Linked to existing CLI idea" --json

curl -sS -X POST http://127.0.0.1:17892/submissions/77777777-7777-7777-7777-777777777777/complete \
  -H 'content-type: application/json' \
  -d '{"idea":"command-line-interface","note":"Linked to existing CLI idea"}'
```

Fail one pending submission when it should not become an idea:

```sh
lexinet submission fail 88888888-8888-8888-8888-888888888888 --note "Duplicate capture" --json

curl -sS -X POST http://127.0.0.1:17892/submissions/88888888-8888-8888-8888-888888888888/fail \
  -H 'content-type: application/json' \
  -d '{"note":"Duplicate capture"}'
```

Expected result: completed submissions move to `status: "enriched"` with an `idea_id`; failed submissions move to `status: "failed"` with the note preserved. Host DB integration for these transitions is operator-verified because the sandbox must not connect to the host Postgres instance.

## Search Strategy Workflow

The daily-driver strategy is server-side config. The macOS client intentionally sends only `q`, `limit`, and `scope=all`; it does not send `strategy`, `fuzzy`, or `threshold`, so no Swift request change is required when the service default changes.

Seed or confirm the target idea exists:

```sh
lexinet idea add "Redis" --slug redis --summary "In-memory data structure store" --json
lexinet name add redis "Redis Serialization Protocol" --kind alias --json
lexinet def add redis "Redis is an in-memory data structure server commonly used as a cache and message broker." --primary --json
```

Verify exact ranking above fuzzy recall over HTTP:

```sh
curl -sS 'http://127.0.0.1:17892/search?q=redis&scope=all&limit=5' | jq '{strategy, provenance, results: [.results[] | {name: .idea.canonical_name, match_type, matcher, score, provenance}]}'
curl -sS 'http://127.0.0.1:17892/search?q=redos&scope=all&limit=5' | jq '{strategy, provenance, results: [.results[] | {name: .idea.canonical_name, match_type, matcher, score, provenance}]}'
```

Expected result: the exact `redis` query returns `Redis` first with `match_type: "exact"` and `matcher: "exact"`; the misspelled `redos` query still returns `Redis` through a fuzzy matcher. The response includes top-level `strategy: "daily-driver"`, response `provenance`, and per-result `provenance`.

Verify the compatibility mapping over HTTP:

```sh
curl -sS 'http://127.0.0.1:17892/search?q=redos&strategy=legacy&scope=all&limit=5' | jq '{strategy, fuzzy, total, results: [.results[] | {name: .idea.canonical_name, match_type, matcher}]}'
curl -sS 'http://127.0.0.1:17892/search?q=redos&strategy=legacy&fuzzy=true&threshold=0.3&scope=all&limit=5' | jq '{strategy, fuzzy, provenance, results: [.results[] | {name: .idea.canonical_name, match_type, matcher, provenance}]}'
```

Expected result: `strategy=legacy` keeps fuzzy disabled by default, while `fuzzy=true` and `threshold=0.3` override the selected legacy strategy for that request.

Verify CLI provenance visibility:

```sh
lexinet search redis --json | jq '{strategy, provenance, first: .results[0] | {name: .idea.canonical_name, match_type, matcher, provenance}}'
lexinet search redos --json | jq '{strategy, provenance, first: .results[0] | {name: .idea.canonical_name, match_type, matcher, provenance}}'
lexinet search redos --strategy legacy --no-fuzzy --json | jq '{strategy, fuzzy, total}'
lexinet search redos --strategy legacy --fuzzy --threshold 0.3 --json | jq '{strategy, fuzzy, provenance, first: .results[0] | {name: .idea.canonical_name, match_type, matcher, provenance}}'
```

Expected result: CLI JSON exposes the same top-level strategy/provenance and per-hit matcher/provenance fields as HTTP. JSONL mode remains result-only by design, so use `--json` for strategy-level provenance.

Verify menu-bar default behavior on macOS:

```sh
cd clients/macos
swift run LexinetMenuBar
```

Expected result: searching `redos` from the menu-bar popover returns the `Redis` idea without adding Swift request parameters or rebuilding after a server-only strategy config change. If `search.default_strategy` or `search.strategies.*` is changed in the config, restart `lexinet serve`; the running service does not hot reload strategy config.

## macOS Menu-Bar Workflow

Run on macOS with Swift available:

```sh
lexinet serve --tailnet off
cd clients/macos
swift build
swift test
swift run LexinetMenuBar
```

Acceptance checks:

- A `Lexinet` item appears in the macOS menu bar.
- Searching `TUI` returns the standalone TUI entry and displays names, definition text when present, topic summaries, and acronym-link summaries.
- Searching `Terminal User Interface` returns the expansion entry and displays the reverse acronym link.
- Searching a misspelled daily-driver query such as `redos` returns the intended `Redis` idea using the server default strategy; no Swift request change is required.
- Stopping `lexinet serve` and searching again shows the service-unavailable hint instead of terminating the menu-bar process.
- Clicking `+` opens capture mode; `Submit to Agent` stays disabled until the idea field contains non-whitespace text.
- Submitting a term queues it, shows `Queued for enrichment`, and returns to search; the queued row is visible through `lexinet submission list --status pending --json` against the host service.
- With the service stopped, capture submit shows an inline error and preserves the typed idea/context.
- The direct `Submit (direct add later)` placeholder remains disabled and does not write ideas.
- Start-at-login setup follows `clients/macos/README.md` and does not require credentials or localhost OAuth callbacks.

## Sandbox Verification

For sandbox builds, use workspace-backed Go temp/cache paths:

```sh
mkdir -p /workspace/.gotmp /workspace/.go-cache
gofmt -w $(git ls-files '*.go')
GOTMPDIR=/workspace/.gotmp GOCACHE=/workspace/.go-cache go build ./cmd/lexinet
GOTMPDIR=/workspace/.gotmp GOCACHE=/workspace/.go-cache go vet ./...
GOTMPDIR=/workspace/.gotmp GOCACHE=/workspace/.go-cache go test ./...
```

For the submissions capture documentation slice, run the required checks explicitly:

```sh
mkdir -p /workspace/.gotmp-test /workspace/.gotmp-vet /workspace/.gotmp-build /workspace/.go-cache
GOTMPDIR=/workspace/.gotmp-test GOCACHE=/workspace/.go-cache go test ./...
GOTMPDIR=/workspace/.gotmp-vet GOCACHE=/workspace/.go-cache go vet ./...
GOTMPDIR=/workspace/.gotmp-build GOCACHE=/workspace/.go-cache go build ./cmd/lexinet
cd clients/macos && swift build
cd clients/macos && swift test
```

If `swift` is not installed in the sandbox, record macOS build/test/menu-bar launch verification as the operator gate above instead of bypassing with host mutations. Host DB integration for submission create/list/complete/fail is operator-verified; the sandbox verification is limited to Go tests/vet/build and Swift package checks when the toolchain exists.

For the search strategy closeout slice, also run:

```sh
mkdir -p /workspace/.gotmp-build /workspace/.gotmp-vet /workspace/.gotmp-test /workspace/.go-cache
GOTMPDIR=/workspace/.gotmp-build GOCACHE=/workspace/.go-cache go build ./...
GOTMPDIR=/workspace/.gotmp-vet GOCACHE=/workspace/.go-cache go vet ./...
GOTMPDIR=/workspace/.gotmp-test GOCACHE=/workspace/.go-cache go test ./...
cd clients/macos && swift build
cd clients/macos && swift test
```

If `swift` is unavailable, record the exact `swift --version` failure and leave menu-bar launch/typo-recall validation as the host Apple-device gate above.

2026-07-18 sandbox result for the search strategy closeout slice:

- `GOTMPDIR=/workspace/.gotmp-build GOCACHE=/workspace/.go-cache go build ./...` passed.
- `GOTMPDIR=/workspace/.gotmp-vet GOCACHE=/workspace/.go-cache go vet ./...` passed.
- `GOTMPDIR=/workspace/.gotmp-test GOCACHE=/workspace/.go-cache go test ./...` passed.
- `swift build` and `swift test` were not run because `swift --version` failed with `/usr/bin/bash: line 1: swift: command not found` in this Linux sandbox.
- Host DB search acceptance remains operator-run against the real Postgres-backed service using the Search Strategy Workflow above; it covers exact `redis` ranking above fuzzy candidates, misspelled `redos` recall through the menu-bar default request shape, and strategy/provenance visibility through HTTP and CLI JSON.

2026-07-18 sandbox result for the docs/version submissions slice:

- `GOTMPDIR=/workspace/.gotmp-test GOCACHE=/workspace/.go-cache go test ./...` passed.
- `GOTMPDIR=/workspace/.gotmp-vet GOCACHE=/workspace/.go-cache go vet ./...` passed.
- `GOTMPDIR=/workspace/.gotmp-build GOCACHE=/workspace/.go-cache go build ./cmd/lexinet` passed.
- `GOTMPDIR=/workspace/.gotmp-version GOCACHE=/workspace/.go-cache go run ./cmd/lexinet version --json` returned `0.2.0-dev`.
- `swift build` and `swift test` were not run because `swift --version` failed with `swift: command not found` in this Linux sandbox.
- Host DB submission integration remains operator-verified: run the capture submission CLI/curl create, list pending, complete, and fail steps above against the Postgres-backed host service.
