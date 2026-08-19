# In CI and in a deploy

*[По-русски](ci-and-deploy.ru.md)*

**Use this when** wiring the tool into a pipeline, where nobody is reading the
output and the exit code is the whole conversation.

## The exit codes

| Code | Meaning | The right reaction |
|---|---|---|
| 0 | done, or there was nothing to do | carry on |
| 1 | the run failed: bad SQL, broken connection | wake somebody up |
| 2 | the command was not understood | fix the pipeline |
| 3 | migrations are pending (`status --check`) | deploy |
| 4 | files and journal disagree | a person has to look |
| 5 | another migrator holds the lock | retry in a minute |
| 6 | refused by a guard | fix the migration, not the database |

The distinction between 5 and 1 is the point of having a table at all: in CI the
right reaction to "another deploy holds the lock" is to wait a minute, and the
right reaction to "the SQL is wrong" is not.

## Check the files without a database

`validate --offline` parses the files, checks the names, the pairing, the
directives and the checksums it can compute without connecting. It belongs in
the same job as the tests:

```console
$ migrator validate --offline
  No problems found.
```

`--strict` promotes the warnings — a missing down file, for instance — to
errors.

## Is a deploy needed?

```console
$ migrator status --check
  The journal "public.schema_migrations" does not exist yet: nothing has been applied.
  "migrator up" will create it.

  VERSION          NAME                         STATUS       APPLIED AT            TOOK
  1                create_users                 pending      -                     -
  2                add_index                    pending      -                     -

  0 applied, 2 pending.  No current version.
migrator: 2 migration(s) pending
exit 3
```

`status` takes no lock and creates nothing, so this is safe to run against a
replica or with a read-only role.

```bash
if migrator status --check; then
    echo "nothing to do"
elif [ $? -eq 3 ]; then
    migrator up
else
    exit 1     # 4 means drift: do not deploy over it
fi
```

## Apply

```console
$ migrator up
INF migrator: migration applied direction=up name=create_users took=1ms version=1
INF migrator: migration applied direction=up name=add_index took=0s version=2
  applied    1_create_users  1ms
  applied    2_add_index  0s

  Done. 2 applied in 15ms. Current version 2.
```

Afterwards `status --check` exits 0, and running `up` again is a no-op that also
exits 0. A pipeline that runs `migrator up` on every release does not need to
know whether there was anything to apply.

## Machine-readable output

Every top-level object carries `"format": 1`. Adding a field does not change it;
removing one or changing what one means does, and that is a CHANGELOG entry of
its own — so a consumer can compare rather than parse.

```console
$ migrator status --json
{
  "format": 1,
  "schema": "public",
  "table": "schema_migrations",
  "initialised": true,
  "current_version": 2,
  "pending": 0,
  "entries": [
    {
      "version": 1,
      "name": "create_users",
      "state": "applied",
      "applied_at": "2026-08-19T19:52:58Z",
      "checksum": "a4dae7e718992086427b3e4e7c600951974e33a25ca8a04f1dcd408c6d478878"
    },
    {
      "version": 2,
      "name": "add_index",
      "state": "applied",
      "applied_at": "2026-08-19T19:52:58Z",
      "checksum": "e5329f66be211a27be111c920f3c1baa92fb128c507865b3044730eeb7a20ef7"
    }
  ]
}
```

**stdout carries the answer and nothing else.** Under `--json` the commentary on
stderr becomes JSON lines too, so a consumer parsing stderr does not break on the
first progress line either. `migrator status --json | jq -r .current_version` is
safe; so is `migrator up --json 2>>deploy.log | jq .`.

## Two deploys at once

The tool serialises runs on a PostgreSQL advisory lock, taken before the journal
table is created. A second run waits, and then gives up with a code that says
retrying is the right move:

```console
$ migrator up
migrator: another migration run holds the lock: waited 2s (advisory lock 1835496052/142344567)
exit 5
```

```console
$ migrator locks
  Lock     1835496052/142344567  (derived from public.schema_migrations)

  pid 176314  migrator  on uc_lock5  active for 2s
    SELECT pg_sleep(6);

  To end one:  SELECT pg_terminate_backend(176314);
```

`--advisory-lock-timeout` sets how long to wait; the default is 30s. The lock is
session-level, so a process that dies releases it — there is no lock row to clean
up by hand.

**A transaction-mode connection pooler breaks this silently**, so the tool checks
for one and refuses rather than pretending. Point migrations at the database
directly, or at a session-mode port.

## In a container

```bash
docker run --rm \
  -e MIGRATOR_DSN='postgres://app:secret@db:5432/shop?sslmode=disable' \
  -e MIGRATOR_ENV=production \
  -v "$PWD/migrations:/migrations" \
  ghcr.io/efureev/migrator:2 up
```

`MIGRATOR_ENV=production` turns on the guards described in
[Guards in production](production-guards.md): `down` and `wipe` become
unavailable, with no flag that overrides it.

Note which settings have **no** environment variable, on purpose: `--allow-down`,
`--allow-wipe` and `--confirm`. A variable is inherited by every process in a
container and every step of a CI job; a flag is typed for one run.

In Kubernetes this is an init container or a Job — it runs, it exits, and the
exit code is what the orchestrator acts on. There is no daemon and no
long-running process by design.

## A GitHub Actions job

```yaml
- name: Check the migration files
  run: migrator validate --offline --strict

- name: Show what would happen
  run: migrator up --dry-run
  env:
    MIGRATOR_DSN: ${{ secrets.MIGRATOR_DSN }}

- name: Apply
  run: migrator up --max-lock-level share-update-exclusive
  env:
    MIGRATOR_DSN: ${{ secrets.MIGRATOR_DSN }}
    MIGRATOR_ENV: production
```

`--max-lock-level` is what turns the dry run from something a human reads into
something the pipeline enforces — see
[Making a schema change safely](safe-schema-change.md).

## Next

- [Guards in production](production-guards.md)
- [Rolling back, and repairing the journal](rollback-and-repair.md) — exit 4, in
  detail
