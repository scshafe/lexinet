# ADR 0003 release-gate evidence: study over the native Tailnet listener

**Status: NOT YET RUN.** Fill this in after the stack is deployed and
`lexinet-study` is enrolled ([../deploy/README.md](../deploy/README.md)). The
deterministic `ListenTLS` failure test in `internal/tailnet` is wrapper cleanup
only and is not evidence for any row below. Panel side:
scshafe/embedded-house-panel#21. Record command output with secrets, tokens and
database contents removed.

Date / operator / lexinet commit / tailnet policy revision (infra #19):

| # | Check | Expected | Result | Evidence |
|---|-------|----------|--------|----------|
| 1 | Panel node (only tag `tag:house-panel`) `POST /api/v1/study/sessions` for profile `cole` | 2xx, session created/resumed | | |
| 2 | Same node `POST .../sync` | 2xx | | |
| 3 | Same node, profile not in `allowed_profile_refs` | refused (structured study error) | | |
| 4 | Untagged node, same request | refused | | |
| 5 | Node with a different tag, same request | refused | | |
| 6 | Tagged node with a request carrying `Authorization` | refused | | |
| 7 | Tagged node, any other route (`GET` session, `/healthz`, `/version`, vocabulary, operator) | not served (study error schema) | | |
| 8 | Revoke live: remove the grant (or retag the node) while a session exists, then request | refused on the next request, no restart | | |
| 9 | Native certificate: handshake to `lexinet-study.<tailnet>:443` | valid Tailscale-issued cert for that name | | |
| 10 | Hostname: node name is `lexinet-study`, tag set is exactly `tag:lexinet-study`, no Funnel | confirmed in the admin console / `tailscale status` | | |
| 11 | Loopback API unreachable from the tailnet (port 17892 not listening on the node) | confirmed | | |
