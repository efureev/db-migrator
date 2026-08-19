# Adopting an existing database

*[По-русски](adopt-existing-database.ru.md)*

**Use this when** the schema already exists — built by hand, by golang-migrate,
or by version 1 of this tool — and you want this tool to take over without
re-running anything.

Without it, `up` would try to apply migration 1 against tables that are already
there. The tool notices and refuses rather than trying:

```console
$ migrator up
migrator: the journal table belongs to another tool: "public"."schema_migrations" exists and has no "checksum" column, so it is not this tool's journal (golang-migrate's is version+dirty) — see "migrator adopt"
exit 6
```

Exit code 6 means refused by a guard: retrying will not help, and the fix is not
in the database.

## The one risk, stated plainly

`adopt` writes the journal that *would* have existed had this tool built the
schema, and runs no migration SQL at all. It takes your word that the files
match what is in the database. That word is the whole risk.

So check first, on a copy if you can:

```bash
pg_dump --schema-only "$PRODUCTION" > before.sql
# apply the migration files to an empty database, dump it, diff the two
```

And the rows are marked forever. `status` shows them as `(adopted)`, because
"applied" and "we were told it was applied" are different claims and only one of
them was observed here.

## From a golang-migrate journal

golang-migrate keeps a `schema_migrations (version bigint, dirty boolean)`. Point
`adopt` at it and it reads the version itself.

Dry-run first. It lists what would be written and writes nothing:

```console
$ migrator adopt --from-golang-migrate --dry-run
  adopted    1_create_users  0s
  adopted    2_create_orders  0s

  Dry run: 2 row(s) would be written, nothing was. Current version 2.
```

Then for real:

```console
$ migrator adopt --from-golang-migrate
INF migrator: moved the old journal aside from=schema_migrations to=schema_migrations_pre_v2
INF migrator: adopted baseline=2 database=uc_adopt recorded=2
  adopted    1_create_users  0s
  adopted    2_create_orders  0s

  Done. 2 applied in 21ms. Current version 2.
```

The old journal is **renamed, not dropped**: `schema_migrations_pre_v2`. Going
back to the old tool has to stay possible for another day or so. Drop it
yourself, later, when you are sure.

```console
$ migrator status
  VERSION          NAME                         STATUS       APPLIED AT            TOOK
  1                create_users (adopted)       applied      2026-08-19 22:48:39   -
  2                create_orders (adopted)      applied      2026-08-19 22:48:40   -
  3                add_orders_total             pending      -                     -

  2 applied, 1 pending.  Current version 2.
```

Only what was genuinely pending runs:

```console
$ migrator up
INF migrator: migration applied direction=up name=add_orders_total took=1ms version=3
  applied    3_add_orders_total  1ms

  Done. 1 applied in 12ms. Current version 3.
```

### A dirty journal is refused

golang-migrate marks a migration that started and did not finish:

```console
$ migrator adopt --from-golang-migrate
migrator: the existing journal is marked dirty: version 2 is marked dirty, so what it left behind is unknown — resolve it with the old tool first
exit 6
```

Nothing is written. Adopting here would freeze an unknown schema state as the
correct one — resolve it with the tool that made it, then come back.

## From a schema built by hand

When there is no journal at all, say how far the database has got:

```console
$ migrator adopt --baseline 2 --confirm shop
INF migrator: adopted baseline=2 database=shop recorded=2
  adopted    1_create_users  0s
  adopted    2_create_orders  0s

  Done. 2 applied in 12ms. Current version 2.
```

Everything at or below the baseline is recorded as applied; everything above
stays pending.

`--confirm <database>` is required outside development. `adopt` does not touch
the schema, but it writes a claim into the database that nobody will re-check
later, and a typo in the database name is what saves you from writing it into
the wrong one.

## Flags worth knowing

| Flag | What it is for |
|---|---|
| `--baseline <v>` | record every migration up to and including `v` |
| `--from-golang-migrate` | read the version out of a `(version, dirty)` journal and move that table aside |
| `--dry-run` | list what would be recorded and record nothing |
| `--confirm <db>` | the database name; required outside development |
| `--force` | adopt over a journal that already has rows |

`--force` exists for the case where a previous attempt got half way. Without it,
a non-empty journal is refused: adoption is an operation for a database this
tool has not seen before.

## Next

- [In CI and in a deploy](ci-and-deploy.md)
- [Making a schema change safely](safe-schema-change.md)
