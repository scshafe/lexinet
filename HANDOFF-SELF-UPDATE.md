# HANDOFF — menu-bar self-update button (queued 2026-07-17, build next session)

Operator request (verbatim intent): "a small button in the lexinet gui that can scan for
updates and attempt to run the updating process in an automated typical install/update way —
so then I can manually install just one more time on the remote machine and then can use
that to update in the future."

## Operational facts (verified at handoff time)

- Canonical remote: `git@github.com:scshafe/lexinet.git` (private). `main` is the release
  line and is PUSHED CURRENT (local == origin @23e1c21, capture arc included).
- Satellite install shape (MacBook Air, operator-installed): clone at `~/lexinet`, built via
  `swift build` in `clients/macos`, menu-bar app kept alive by a per-user LaunchAgent
  (label `dev.lexinet.menubar` — VERIFY on the machine, the operator ran the install
  prompt), endpoint `http://elrics-mac-mini:17892` via UserDefaults `endpoint` /
  `LEXINET_ENDPOINT`. The Air still runs the PRE-capture build; the operator will do ONE
  more manual update after this feature ships, then the button takes over.
- Mini (source machine): the app runs out of THIS repo working copy, which Mission Control
  develops in. `git fetch` here typically finds nothing (we push, not pull) — the button
  must degrade gracefully (offer rebuild-only, or report "source install — up to date").

## Proposed shape (next session may revise)

1. **Version surface**: show the running build's short commit hash in the popover footer
   (embed at build time or `git rev-parse --short HEAD` from the repo at runtime).
2. **Check**: button runs `git fetch origin` + `git rev-list HEAD..origin/main --count` in
   the repo the binary came from. Repo discovery: walk up from the binary path to `.git`,
   falling back to a UserDefaults `repoPath` written at install.
3. **Update**: if behind, invoke a repo-shipped `clients/macos/update.sh` doing the typical
   manual sequence — `git pull --ff-only` → `swift build -c release` →
   `launchctl kickstart -k gui/$UID/<launchagent-label>` — so the button and manual
   installs share ONE code path.
4. **UX**: async (Process off the main actor), progress + errors surfaced in the popover;
   a failed update leaves the current install running (`--ff-only` never half-applies).

## Constraints

- Button-triggered ONLY — no auto-update timer unless the operator asks.
- Pulling executes repo code: same trust boundary as the manual install (own private repo
  over the satellite's own GitHub SSH access, which exists — the operator cloned it).
- Unrelated: the capture form's direct **Submit** button stays disabled (reminder stub).

## Build route

Real-app doctrine (speed over reviews). Small enough for a direct host build or a single
dissolved-arc child; operator pre-approved menubar work. After shipping: push main, then
hand the operator the one-time Air update line:
`cd ~/lexinet && git pull && cd clients/macos && swift build && launchctl kickstart -k gui/$(id -u)/dev.lexinet.menubar`
