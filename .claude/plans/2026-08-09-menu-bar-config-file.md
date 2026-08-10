# Plan: Menu-bar config file

**Goal:** Make the macOS menu-bar client resolve its server URL from `~/.config/lexinet/config.toml` so the LaunchAgent no longer contains a machine-specific endpoint.
**Out of scope:** Server or Docker changes, submission POST verification, running `clients/macos/update.sh`, and changing the Go CLI's macOS config-path behavior.

## Context

The menu-bar app currently resolves `LEXINET_ENDPOINT`, then the `endpoint` user default, then loopback in `clients/macos/Sources/LexinetMenuBar/LexinetMenuBarApp.swift`. This machine's loaded LaunchAgent currently supplies `LEXINET_ENDPOINT`. The new tailnet endpoint is `https://lexinet.tail49736b.ts.net`.

## Steps

- [x] **Step 1: Add a tested endpoint configuration resolver**
  - Verify: `cd clients/macos && swift build --target LexinetClient`, then compile and run a standalone resolver smoke test with the local Swift toolchain.
  - Notes: Preserve an explicit `LEXINET_ENDPOINT` override; otherwise read `[server].url` from `LEXINET_CONFIG` or `~/.config/lexinet/config.toml`. Surface invalid present configuration instead of silently falling back.

- [x] **Step 2: Wire and document menu-bar configuration failures**
  - Verify: `cd clients/macos && swift build`
  - Notes: The UI must show a configuration error and must not enable capture when endpoint resolution fails.

- [x] **Step 3: Migrate this machine and verify the live app**
  - Verify: the reloaded `dev.lexinet.menubar` has no `LEXINET_ENDPOINT`, and a post-restart menu-bar search for `tui` renders the real result.
  - Notes: Write the endpoint to `~/.config/lexinet/config.toml`, remove the LaunchAgent environment entry and old user default, reload the job, and do not send a submission.

## Done criteria

The LexinetClient target and standalone resolver smoke test pass (the local Command Line Tools installation cannot import `XCTest`); the installed LaunchAgent has no endpoint; the restarted menu-bar app reads the new HTTPS URL from `~/.config/lexinet/config.toml`; menu-bar and CLI searches for `tui` both succeed.

**Amended 2026-08-09:** The installed Command Line Tools contain neither `XCTest` nor `Testing`, so `swift test` cannot compile the pre-existing test suite. Preserve the XCTest coverage in the repo and use a standalone runtime smoke test locally instead of expanding scope to install Xcode.
