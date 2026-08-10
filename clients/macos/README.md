# lexinet macOS menu-bar client

This directory contains an optional native macOS menu-bar helper for lexinet. It is isolated from the Go module, Docker deployment, credentials, and OAuth flows.

The app queries the configured lexinet service, opens a menu-bar popover, searches ideas, captures submissions for later enrichment, and renders canonical name, names, definition, topics, and acronym-link summaries when the HTTP API returns them. If configuration is invalid, the service is down, or the service returns a non-2xx response, the popover shows a clear error and keeps running.

## Requirements

- macOS 13 or newer.
- Xcode command line tools or Xcode with Swift 5.9+.
- A reachable lexinet service.

## Start lexinet

From the repository root or wherever the `lexinet` binary is installed:

```sh
lexinet serve --tailnet off
```

Without client configuration, the menu-bar app uses `http://127.0.0.1:17892`.

## Configure the service endpoint

The normal endpoint setting is `[server].url` in `~/.config/lexinet/config.toml`:

```toml
[server]
url = "https://lexinet.example.ts.net"
```

Endpoint precedence is `LEXINET_ENDPOINT`, then `[server].url` in the file named by `LEXINET_CONFIG` or `~/.config/lexinet/config.toml`, then the legacy `endpoint` user default, then loopback. `LEXINET_ENDPOINT` is intended for an explicit launch or test override; a LaunchAgent does not need to contain the server address.

The app reads endpoint configuration when it starts. Restart it after changing the file. A present but unreadable file, an invalid `server.url`, or an invalid environment override is shown as a configuration error instead of silently falling back to another endpoint.

## Build

```sh
cd clients/macos
swift build
```

Run the non-UI decoding tests:

```sh
cd clients/macos
swift test
```

The Swift package is not referenced by `go.mod`, Docker files, or Go build commands.

## Run

```sh
cd clients/macos
swift run LexinetMenuBar
```

Click the `Lexinet` menu-bar item, type a term, and press Return or click Search. The app sends `q`, `limit`, and `scope=all` only; it deliberately omits `strategy`, `fuzzy`, and `threshold` so lookup behavior follows the server's configured default search strategy. When the service is unavailable, the popover displays a start hint: `lexinet serve --tailnet off`.

Search strategy changes are service-side. After changing `search.default_strategy` or `search.strategies.*` in the lexinet config, restart `lexinet serve`; the running service does not hot reload strategy config, and the menu-bar binary does not need rebuilding.

## Capture Flow

Click the `+` button in the popover to switch from search to capture mode. Enter an `Idea (word or acronym)` and, optionally, add multiline `Context / details`.

The `Submit to Agent` button is disabled until the idea field has non-whitespace text. When clicked, it posts a pending submission to `/submissions`, shows inline progress, then shows `Queued for enrichment` and returns to search after a brief confirmation. If the service is down or returns validation errors, the error stays inline and the typed idea/context remain in the form.

The disabled `Submit (direct add later)` button is visible only as a future direct-write placeholder; the menu-bar app does not write ideas directly.

Manual capture walkthrough:

1. Start `lexinet serve --tailnet off` and run `swift run LexinetMenuBar` from this directory.
2. Open the `Lexinet` menu-bar popover and click `+`.
3. Confirm `Submit to Agent` is disabled while `Idea (word or acronym)` is empty or whitespace-only.
4. Type `TUI` in the idea field and add optional context such as `Seen in terminal UI notes`.
5. Confirm `Submit to Agent` becomes enabled while `Submit (direct add later)` remains visible and disabled.
6. Click `Submit to Agent`; expect `Queued for enrichment`, then automatic return to search.
7. Confirm the queue from the service side with `lexinet submission list --status pending --json` or `curl -sS 'http://127.0.0.1:17892/submissions?status=pending'`.
8. Stop the service, reopen capture mode, enter idea/context, and click `Submit to Agent`.
9. Confirm the inline service error is shown and the typed idea/context are preserved for retry.

## Install

For a local developer install, build a release binary and place it under a user-owned app/bin location:

```sh
cd clients/macos
swift build -c release
mkdir -p ~/Applications/LexinetMenuBar
cp .build/release/LexinetMenuBar ~/Applications/LexinetMenuBar/
```

Run it with:

```sh
~/Applications/LexinetMenuBar/LexinetMenuBar
```

For a fully packaged `.app`, open the package in Xcode and create an app archive/signing configuration as desired. This repo intentionally does not add signing identities or credentials.

## Start At Login

To start the helper after login:

1. Open macOS System Settings.
2. Go to General > Login Items & Extensions.
3. Add `~/Applications/LexinetMenuBar/LexinetMenuBar` or the packaged `.app` if you created one.
4. Keep the endpoint in `~/.config/lexinet/config.toml`; the login item or LaunchAgent only needs to launch the binary.
5. Keep `lexinet serve --tailnet off` managed separately, for example with your preferred shell profile, launchd user agent, or manual startup.

## Self-Update

The popover footer shows the checkout's short commit hash and a `Check for Updates` button. Checking runs `git fetch origin` and counts `HEAD..origin/main` in the repository the running binary came from; when commits are available, `Update Now` appears.

`Update Now` runs the repo-shipped `clients/macos/update.sh --no-restart` — the same script a manual update uses — then restarts the app:

- `git pull --ff-only` (a failed pull or build aborts before anything is replaced, leaving the current install running)
- `swift build -c release`
- if a LaunchAgent plist (`~/Library/LaunchAgents/dev.lexinet.menubar.plist`) points at a different binary path, the fresh binary is installed there atomically
- restart: when the running instance is the LaunchAgent's process, the app restarts itself via `launchctl kickstart -k`; a manually launched instance shows `Updated — quit and relaunch the app to finish` instead

Manual update (same code path as the button):

```sh
cd ~/lexinet && git pull --ff-only && ./clients/macos/update.sh
```

Repository discovery walks up from the executable path, which covers running out of a clone's `.build` tree. If the binary was copied elsewhere (e.g. `~/Applications`), point the app at the clone with `defaults write dev.lexinet.menubar repoPath ~/lexinet` or the `LEXINET_REPO_PATH` environment variable. A LaunchAgent with a different label is supported via `LEXINET_LAUNCHAGENT_LABEL`.

Notes:

- Updates are button-triggered only; there is no background timer.
- On the source machine (the checkout Mission Control develops in), fetch typically finds nothing and the footer reports `Up to date`.
- Pulling executes repository code — the trust boundary is the same as a manual `git pull` of your own private repo.

## Operator Verification

Sandbox verification cannot launch the macOS menu-bar UI. On Apple Silicon, verify:

1. `lexinet serve --tailnet off` is running and responds on `127.0.0.1:17892`.
2. `swift run LexinetMenuBar` starts a `Lexinet` menu-bar item.
3. Clicking the menu-bar item opens the popover.
4. Searching a known term returns an idea and shows names plus definition.
5. Searching a misspelled daily-driver query, for example `redos` with a `Redis` idea in the service, returns the intended idea without any app-side strategy parameters.
6. Topic and acronym-link summaries appear for ideas whose HTTP responses include `topics` and `acronym_links`.
7. Stopping the service and searching again shows the service-unavailable state with the start hint.
8. Clicking `+` opens capture mode and `Submit to Agent` remains disabled for an empty or whitespace-only idea.
9. Entering an idea and context, then clicking `Submit to Agent`, queues a pending submission and returns to search after the success confirmation.
10. Stopping the service, entering capture text, and clicking `Submit to Agent` shows an inline service error without clearing the typed text.
11. `Submit (direct add later)` is visible and disabled; no direct idea write is available from the app.

The host DB integration check is operator-verified: after a successful capture, `lexinet submission list --status pending --json` must show the queued term in the same Postgres-backed service used by the menu-bar app.
