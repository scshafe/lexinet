#!/bin/bash
# lexinet menu-bar updater — the ONE update path shared by the in-app button and
# manual installs. Sequence: git pull --ff-only, swift build -c release, sync the
# built binary onto whatever path the LaunchAgent runs, restart via launchctl.
#
#   update.sh [--no-restart]
#
# --no-restart skips the launchctl kickstart; the menu-bar app passes it and
# restarts itself, so the script is never killed mid-run by its own caller dying.
#
# Environment:
#   LEXINET_LAUNCHAGENT_LABEL  launchd label (default dev.lexinet.menubar)
#
# Failure behavior: any failing step aborts before the restart, so a failed
# update leaves the currently installed build running untouched.

set -euo pipefail

export PATH="/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin:/opt/homebrew/bin"
export GIT_TERMINAL_PROMPT=0

NO_RESTART=0
for arg in "$@"; do
  case "$arg" in
    --no-restart) NO_RESTART=1 ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

LABEL="${LEXINET_LAUNCHAGENT_LABEL:-dev.lexinet.menubar}"
CLIENT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$CLIENT_DIR/../.." && pwd)"

if [ ! -e "$REPO_ROOT/.git" ]; then
  echo "error: $REPO_ROOT is not a git checkout" >&2
  exit 1
fi

cd "$REPO_ROOT"
BEFORE="$(git rev-parse --short HEAD)"
echo "==> lexinet update: $REPO_ROOT @ $BEFORE"

echo "==> git pull --ff-only"
git pull --ff-only

AFTER="$(git rev-parse --short HEAD)"
if [ "$BEFORE" = "$AFTER" ]; then
  echo "==> already at $AFTER; rebuilding anyway"
else
  echo "==> advanced $BEFORE -> $AFTER"
fi

echo "==> swift build -c release"
cd "$CLIENT_DIR"
swift build -c release
RELEASE_BIN="$(swift build -c release --show-bin-path)/LexinetMenuBar"

if [ ! -x "$RELEASE_BIN" ]; then
  echo "error: build product missing at $RELEASE_BIN" >&2
  exit 1
fi

# If a LaunchAgent runs this app from some other path (e.g. ~/Applications or a
# debug build product), install the fresh binary there. Copy to a temp file and
# mv so the running process's backing file is replaced atomically, never
# truncated in place.
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
if [ -f "$PLIST" ]; then
  TARGET="$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$PLIST" 2>/dev/null \
         || /usr/libexec/PlistBuddy -c 'Print :Program' "$PLIST" 2>/dev/null \
         || true)"
  if [ -n "$TARGET" ] && [ "$TARGET" != "$RELEASE_BIN" ] && [ -f "$TARGET" ]; then
    echo "==> installing binary onto LaunchAgent path $TARGET"
    TMP="$(mktemp "$(dirname "$TARGET")/.LexinetMenuBar.XXXXXX")"
    cp "$RELEASE_BIN" "$TMP"
    chmod 755 "$TMP"
    mv -f "$TMP" "$TARGET"
  fi
fi

if [ "$NO_RESTART" = "1" ]; then
  echo "==> built $AFTER (restart skipped; caller restarts the app)"
  exit 0
fi

if launchctl print "gui/$(id -u)/$LABEL" >/dev/null 2>&1; then
  echo "==> restarting $LABEL"
  launchctl kickstart -k "gui/$(id -u)/$LABEL"
  echo "==> updated to $AFTER and restarted"
else
  echo "==> updated to $AFTER; no LaunchAgent '$LABEL' found — quit and relaunch the app manually"
fi
