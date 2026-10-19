# Moving the frozen stack's data

Source: the frozen stack's Postgres 16 data directory on `cole-laptop`,
`~/infra/stacks/lexinet/state/db` (about 48 MB, last written about
2026-10-04). Target: the new stack's `lexinet-db` (`./state/db`).

**Dump and restore, not adopting the directory.** A logical dump restores into
a fresh volume with the new image's Postgres and the new role/password, with no
dependency on the old image's uid, locale or WAL state. This is an owner/infra
step; the agent cannot reach the laptop.

1. Copy the old state directory first (`cp -a`) as a rollback point. Do not
   write to the original.
2. Start a throwaway `postgres:16` on the **copy**, with the old
   `POSTGRES_USER`/`POSTGRES_DB` (`lexinet`/`lexinet`), no published port:
   `docker run --rm -d --name lexinet-old -v /path/to/copy:/var/lib/postgresql/data postgres:16-bookworm`
3. Record counts: `docker exec lexinet-old psql -U lexinet -d lexinet -Atc "select relname, n_live_tup from pg_stat_user_tables order by 1" > counts-before.txt`
4. Dump: `docker exec lexinet-old pg_dump -U lexinet -d lexinet -Fc --no-owner > lexinet.dump`
   Stop and remove the throwaway.
5. Bring up only `lexinet-db` of the new stack (first deploy creates the empty
   database), then restore:
   `docker exec -i lexinet-db pg_restore -U lexinet -d lexinet --no-owner --clean --if-exists < lexinet.dump`
6. Start `lexinet`; `serve` runs `migrate` and applies anything newer than the
   dump. Compare counts with step 3 (`counts-after.txt`) and spot-check
   `lexinet search`. `pg_trgm` is created by the migrations.
7. Keep `lexinet.dump` and the copy until the first scheduled backup of the new
   stack has been restored once in a drill. Do not commit or paste database
   contents anywhere.

Backups of the new stack are `stack.toml [backup] pg` (a `pg_dump` of the live
database) plus the `state/tsnet` identity; the old stack had none.
