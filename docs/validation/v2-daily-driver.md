# lexinet v2 daily-driver manual acceptance

This walkthrough verifies the topic, acronym-entry, TUI/Terminal User Interface, and menu-bar workflows against a real Postgres-backed service. It is intentionally operator-run: the sandbox must not reach or mutate the host database.

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
- Stopping `lexinet serve` and searching again shows the service-unavailable hint instead of terminating the menu-bar process.
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

If `swift` is not installed in the sandbox, record macOS build/test/menu-bar launch verification as the operator gate above instead of bypassing with host mutations.
