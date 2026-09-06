#!/bin/sh
# scripts/conductor-gate.sh — the release gate Mission Control's Conductor runs
# before and after deploying this repository. The manifest names ONE verb per
# stage; the logic lives here so the manifest stays a list of commands and the
# gate can be run by hand from the repository root.
#
#   test                        go test ./... in the Dockerfile's Go image (no Go on the host)
#   verify HEALTH_URL SIDECAR HOST   after deploy: the health URL answers and
#                               the tailscale sidecar serves HOST to the tailnet only
#
# The Conductor runs each verb via /bin/sh -c in its own per-commit worktree of
# this repository, with a ten-minute cap per command.
set -eu
cd "$(dirname "$0")/.."
verb="${1:-}"
shift || true
case "$verb" in
  test)
    # No Go toolchain on the deploy host: the tests run in the same image the
    # Dockerfile builds from, with module and build caches kept between runs.
    docker run --rm -v "$PWD":/src -w /src \
      -v conductor-gomod:/go/pkg/mod -v conductor-gobuild:/root/.cache/go-build \
      "golang:1.23.4-bookworm" go test ./...
    echo "conductor-gate: go test ok (golang:1.23.4-bookworm)"
    ;;
  verify)
    url="${1:?verify needs the health URL}"
    sidecar="${2:?verify needs the tailscale sidecar container name}"
    host="${3:?verify needs the tailnet host the sidecar must serve}"
    curl -fsS --max-time 8 "$url" > /dev/null
    status="$(docker exec "$sidecar" tailscale funnel status 2>&1)"
    printf '%s\n' "$status" | grep -q "^https://$host (tailnet only)" \
      || { echo "conductor-gate: $sidecar does not serve https://$host tailnet-only:" >&2; printf '%s\n' "$status" >&2; exit 1; }
    if printf '%s\n' "$status" | grep -qi "funnel on"; then
      echo "conductor-gate: Funnel is ON for $host — the tailnet-only rule forbids it" >&2
      printf '%s\n' "$status" >&2
      exit 1
    fi
    echo "conductor-gate: verify ok — $url answers, $host tailnet only"
    ;;
  *)
    echo "usage: scripts/conductor-gate.sh test|verify HEALTH_URL SIDECAR HOST" >&2
    exit 64
    ;;
esac
