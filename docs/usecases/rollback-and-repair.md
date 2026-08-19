# Rolling back, and repairing the journal

*[По-русски](rollback-and-repair.ru.md)*

**Use this when** something went in that should not have, or when the journal
and the files disagree and `up` has stopped with exit code 4.

Two different tools for two different problems, and the difference matters:
`down` changes the schema, `repair` changes only the journal and never the
schema.

## Rolling back

```console
$ migrator down --allow-down --yes
INF migrator: migration applied direction=down name=add_index took=2ms version=2
  reverted   2_add_index  2ms

  Done. 1 rolled back in 25ms.
```

`--allow-down` is required and has no environment variable, on purpose: it says
you meant it, this time. In a production environment `down` is refused outright
and no flag overrides that — see [Guards in production](production-guards.md).

The row is updated, not deleted. History of what ran is worth more than a tidy
table:

```console
$ migrator status
  VERSION          NAME                         STATUS       APPLIED AT            TOOK
  1                create_users                 applied      2026-08-19 22:52:58   1ms
  2                add_index                    rolled back  2026-08-19 22:52:58   2ms

  1 applied, 1 pending.  Current version 1.
```

`--steps n` rolls back n; `--to <version>` rolls back everything above that
version, leaving it in force; `--dry-run` prints the plan and changes nothing.

## Rolling back and re-applying

`redo` does both under one lock — the development half of what `fresh` used to
be:

```console
$ migrator redo --allow-down --yes
  reverted   1_create_users  3ms
  applied    1_create_users  2ms

  Done. 2 rolled back in 21ms. Current version 1.
```

Both directions are checked against `--max-lock-level` *before* either runs.
Refusing on the way back up, after the rollback has already happened, would
leave the database somewhere nobody asked for.

The destructive half is `migrator wipe && migrator up`, which is two commands on
purpose.

## When a file changed after it was applied

Every migration carries a checksum of its text. Editing a file that has already
been applied is refused, loudly and before anything runs — because every database
that applied the old text will never see the new one.

```console
$ migrator status --check
  VERSION          NAME                         STATUS       APPLIED AT            TOOK
  1                create_users                 changed      2026-08-19 22:53:11   2ms
  2                add_index                    rolled back  2026-08-19 22:52:58   2ms

  0 applied, 1 pending, 1 needing attention.  Current version 1.
  Run "migrator validate" for the details.
migrator: migration file changed after it was applied: 1 migration(s) need attention
exit 4
```

```console
$ migrator up
migrator: 1_create_users.up.sql: applied as a4dae7e71899, on disk 6d90659849b0
exit 4
```

`validate` gives the whole picture at once, errors and warnings together:

```console
$ migrator validate
  1_create_users.up.sql: error: applied as a4dae7e71899, on disk 6d90659849b0
  2_add_index.up.sql: warning: not applied

  1 error(s), 1 warning(s).
exit 4
```

There are two ways out, and the first is almost always right:

1. **Put the file back and write a new migration.** What is in the database is
   what the old text produced; the new text has never run anywhere.
2. **Accept the new checksum**, if the edit genuinely changed nothing that runs —
   a comment, whitespace, a rename in a comment. (Line endings alone are already
   handled: the checksum is taken over a normalised form, so a Windows checkout
   with `core.autocrlf=true` does not look like an edit.)

```console
$ migrator repair --rehash 1
INF migrator: repaired operations=1
  rehash 1: a4dae7e71899 -> 6d90659849b0
```

**There is no `--force-checksum` on `up`**, and that is deliberate. A flag on the
command that applies migrations gets reached for in a hurry — at exactly the
moment the loud refusal is doing its job. `repair` names the version, prints both
checksums, and touches nothing else.

## When a migration started and never came back

Only the no-transaction path can leave this: the row is committed before the
statements run and updated after, so a crash in between is visible afterwards
rather than silent.

```console
$ migrator up
migrator: 2_add_index.up.sql:4: FATAL: terminating connection due to administrator command (SQLSTATE 57P01)
exit 1
```

```console
$ migrator status
  VERSION          NAME                         STATUS       APPLIED AT            TOOK
  1                create_users                 applied      2026-08-19 22:55:42   1ms
  2                add_index                    incomplete   2026-08-19 22:55:42   -

  1 applied, 0 pending, 1 needing attention.  Current version 1.
  Run "migrator validate" for the details.
```

Nothing runs until a person decides:

```console
$ migrator up
migrator: migration recorded as started was never confirmed: 2_add_index started at 2026-08-19T22:55:42+03:00 by fureev@MacBook-Pro-Evgenij.local
exit 4
```

**Automatic retry would be wrong here.** A failed `CREATE INDEX CONCURRENTLY`
leaves an invalid index behind, and a second `CONCURRENTLY` will not replace it —
it fails with 42P07. So look at the schema first:

```console
$ psql -c '\d users'
                            Table "public.users"
 Column |  Type  | Collation | Nullable |              Default
--------+--------+-----------+----------+-----------------------------------
 id     | bigint |           | not null | nextval('users_id_seq'::regclass)
 email  | text   |           |          |
Indexes:
    "users_pkey" PRIMARY KEY, btree (id)
    "users_email_idx" btree (email)
```

The index is there and valid, so the work was done and only the bookkeeping was
lost:

```console
$ migrator repair --complete 2
INF migrator: repaired operations=1
  complete 2

$ migrator status
  VERSION          NAME                         STATUS       APPLIED AT            TOOK
  1                create_users                 applied      2026-08-19 22:55:42   1ms
  2                add_index                    applied      2026-08-19 22:55:42   -

  2 applied, 0 pending.  Current version 2.
```

Had the schema shown the migration had *not* taken effect, the other way out is
`repair --discard 2`, which forgets the version so it runs again:

```console
$ migrator repair --discard 2
INF migrator: repaired operations=1
  discard 2
```

A migration that is genuinely idempotent can say so and skip this conversation
entirely:

```sql
-- migrator:no-transaction
-- migrator:retry-safe
```

## Every repair

| Flag | What it does |
|---|---|
| `--rehash <v>` | record the file's current checksum for version `v` |
| `--complete <v>` | mark an interrupted no-transaction migration as finished |
| `--discard <v>` | forget version `v`, so that it runs again |
| `--prune` | forget every recorded version whose file is gone |

`repair` never runs the SQL of a migration and never touches the schema, so it is
available in every environment — including the one where it is needed most.

## Next

- [Guards in production](production-guards.md)
- [Watching a long migration](watching-a-long-migration.md) — how to see the one
  that is about to be interrupted
