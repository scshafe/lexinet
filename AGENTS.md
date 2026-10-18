# lexinet agent contract

lexinet is an idea-centric technical lexicon: ideas (concepts) with many names,
definitions, examples, notes, typed relationships and search, plus the `study.v1`
flash-card API the house panel (`scshafe/embedded-house-panel`, vocabulary app)
consumes. One Go binary, `cmd/lexinet` (`serve`, `migrate`, and JSON-first client
commands), on Postgres 16 with `pg_trgm`. Public, MIT. A scshafe-dev `service` project
(`dev.toml`).

- **Run:** `go build ./cmd/lexinet`; `deploy/docker/docker-compose.yml` runs the
  service and a Postgres on loopback `127.0.0.1:17892` for development. Config is
  `LEXINET_CONFIG` or `os.UserConfigDir()/lexinet/config.toml` (README "Configure").
- **Verify:** `go test ./...` (`dev.toml [verify]`; CI runs it on GitHub-hosted
  runners and never deploys). The storage and API tests start a disposable
  `postgres:16` container through docker (`internal/testsupport/studypostgres`), so
  docker must be available.
- **Contracts:** `contracts/study.v1/` (OpenAPI 3.1, golden examples, `manifest.json`)
  is frozen (`docs/decisions/0001`, `0002`). Requests reject unknown fields; responses
  may only gain optional fields; any change in meaning is a `study.v2`. The panel pins
  the contract by revision and SHA-256 (embedded-house-panel
  `docs/interfaces/lexinet-contract.md`): a contract change needs a matching panel
  re-pin, so name it in the PR.
- **Tailnet study access:** a separate native `tsnet.ListenTLS` listener exposes only
  study create/resume and sync, with a fresh `WhoIs` per request
  (`docs/decisions/0003`). Never put that listener behind a proxy or trust forwarded
  identity headers. The tsnet state directory is an identity secret.
- **Production:** not deployed (`[deploy] lane = "none"`). The infra stack
  `stacks/lexinet` (sidecar `serve`, `--tailnet off`) is frozen on `cole-laptop`:
  restored, not started. Moving lexinet to the runner lane with its native tsnet
  listener is planned work.
- **Secrets and data:** never print or commit tokens, tsnet state, `.env` files, or
  database contents. Test tokens in the tree are canaries.
- **History:** the `study.v1` work (2026-08-16/17) was developed on the Air and
  published here on 2026-10-18; embedded-house-panel's pin `ce42272` is an ancestor
  of `main`.
