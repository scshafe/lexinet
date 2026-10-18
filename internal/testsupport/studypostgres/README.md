<!-- @canonical study_disposable_postgres_test_harness -->

# Disposable study Postgres test harness

This package owns the one reusable lifecycle for study integration databases.
By default it starts a randomly named, loopback-only Postgres 16 Docker
container and removes only that proven-owned container during test cleanup.

`LEXINET_STUDY_TEST_DSN` may instead name a caller-owned disposable database.
That database must be empty and its name must start with
`lexinet_study_test_`. The harness installs a random ownership marker before
allowing destructive migration work and removes only that marker at cleanup.
It never logs DSNs, credentials, raw driver messages, or command arguments.

Callers own schema migration and domain fixture setup. This keeps the database
lifecycle independent from `internal/storage` and lets storage, server, and CLI
integration tests share exactly one safety boundary without an import cycle.
