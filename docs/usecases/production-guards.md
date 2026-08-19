# Guards in production

*[По-русски](production-guards.ru.md)*

**Use this when** deciding what the tool is allowed to do against a database that
real people are using.

The guards are the point of this tool, not a feature of it. Every one of them is
a place where the obvious convenience was refused on purpose.

## Rolling back needs to be asked for

```console
$ migrator down
migrator: down migrations require WithAllowDown

Rolling back changes the schema of a database something is already running against.
Pass the flag once you have decided that is what you want:

  migrator down --steps 1 --allow-down
exit 6
```

## In production it is not available at all

```console
$ migrator down --allow-down --env production
migrator: refused in a production environment: rolling back is not available when the environment is production
exit 6
```

There is no flag that overrides this. If the schema in production has to go
backwards, that is a forward migration written for the purpose, reviewed like any
other — not a rollback typed at speed.

The environment comes from `--env` or `MIGRATOR_ENV`, and when neither is set it
is **inferred** from the DSN: a non-loopback, non-private host or a
production-looking database name reads as production. The bias is deliberate.
Set it explicitly in a deployment and stop relying on the guess:

```bash
export MIGRATOR_ENV=production
```

## Wiping takes three separate keys

`wipe` drops every table, view, sequence, routine and type in the schema, in one
transaction, leaving the schema itself and its extensions alone.

It needs `--allow-wipe` **and** `--confirm <database>`, and the confirmation is
checked against the database you are actually connected to:

```console
$ migrator wipe --allow-wipe --confirm uc_guardz
migrator: confirmation does not name this database: confirmation names "uc_guardz", connected to "uc_guards"
exit 6
```

The typo is the point. `--yes` means "stop asking"; `--confirm <database>` means
"I know which database this is", and those are different statements. The given
confirmation is checked in **every** environment — only whether it is *required*
depends on the environment.

Look first:

```console
$ migrator wipe --allow-wipe --confirm uc_guards --dry-run
dry run: nothing will be dropped
  would drop table public.schema_migrations
  would drop table public.users

  2 objects would be dropped, 0 kept in uc_guards
```

The dry run passes through the **same** gates as the real thing, not fewer:
"what would this destroy" must not become a way to ask without the flags that
make the answer meaningful.

And in production, again, not at all:

```console
$ migrator wipe --allow-wipe --confirm uc_guards --env production
migrator: refused in a production environment: wipe is not available when the environment is production
exit 6
```

By default a database whose name matches `(?i)prod` is protected even outside a
production environment. The pattern is a library option
(`WithWipeProtectPattern`) and has no flag: it is a property of how a tool is
embedded, not of one invocation.

### Objects outside the schema

`DROP ... CASCADE` on a table that a view in **another** schema depends on takes
that view with it, silently. `wipe` walks `pg_depend` first and refuses, listing
what would have gone:

```
  refused: dropping public.users would take reporting.user_summary with it
```

Accepting that is a separate decision, and it is a separate library option —
`WithForceWipe`, not the one that allowed the wipe. Agreeing to erase your own
schema and agreeing to lose somebody else's object are two different things, and
one switch for both would collect the second consent along with the first.

**The CLI has no flag for it at all.** From the command line the way past a
cross-schema dependency is to deal with the dependent object yourself, in the
schema that owns it, which is where somebody knows what it is for.

## Why some settings have no environment variable

`--allow-down`, `--allow-wipe` and `--confirm` deliberately have none. A variable
is inherited by every process in a container and every step of a CI job; a flag
is typed for one run.

`--max-lock-level` does have one (`MIGRATOR_MAX_LOCK_LEVEL`) for the opposite
reason: it is a policy meant to be set once for a pipeline and left there. It is
a restriction, not a permission.

## Rights, checked before the first statement

Before anything runs, the tool checks that the role can create in the schema it
was pointed at:

```
migrator: insufficient privilege: role "app_ro" may not create in schema "public"
exit 6
```

Discovering a missing privilege on the third migration is the worst moment to
learn it, especially when the first two have already applied. `status` and
`version` do not make this check — they create nothing, so they stay usable with
a read-only role.

## What is not guarded, and cannot be

`adopt` writes a claim into the journal that nobody will re-check later, and it
believes you. That is its single risk, and `--confirm` outside development is
the only thing standing there. See
[Adopting an existing database](adopt-existing-database.md).

## Next

- [In CI and in a deploy](ci-and-deploy.md)
- [Making a schema change safely](safe-schema-change.md) — the guard that reads
  the SQL rather than the flags
