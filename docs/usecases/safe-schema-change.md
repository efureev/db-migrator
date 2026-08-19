# Making a schema change safely

*[По-русски](safe-schema-change.ru.md)*

**Use this when** you are about to change a table that something is reading from
right now, and you want to know what the statement will lock before it locks it.

This is the thing the tool is for. Not "this migration is risky" — the lock
mode, the table, how many rows are in it, and whether it rewrites or scans.

## 1. Ask before you run

```console
$ migrator up --dry-run
  Plan  2 migration(s) up

    20260901120000_add_email_index  transactional, 1 statement(s)
      CREATE INDEX users_email_idx ON users (email)
        SHARE on users (~41 200 rows), scans the table
        CREATE INDEX without CONCURRENTLY blocks every INSERT, UPDATE and DELETE
        until the index is built

    20260901130000_widen_status  transactional, 1 statement(s)
      ALTER TABLE orders ALTER COLUMN status TYPE text
        ACCESS EXCLUSIVE on orders (~8 900 rows), REWRITES THE TABLE
        changing a column's type rewrites the table unless the two types are
        binary-coercible (varchar(n) to a wider varchar or to text, and little
        else). The old type is not in the statement, so a rewrite is assumed
```

Row counts come from `pg_class.reltuples`, so they are an estimate and they are
missing entirely until something has `ANALYZE`d the table. Digits are grouped
because the difference between 8 900 000 and 890 000 is the difference between
a warning worth heeding and one worth ignoring, and nobody counts digits.

`--dry-run --json` gives the same plan for a machine.

## 2. Turn the knowledge into a rule

Reading the plan every time works until the day somebody does not. Set a ceiling
and let the tool refuse:

```console
$ migrator up --max-lock-level share-update-exclusive
migrator: 20260901120000_add_email_index.up.sql: statement 1 takes SHARE on users (~41 200 rows), and the limit is SHARE UPDATE EXCLUSIVE; it scans the table. CREATE INDEX without CONCURRENTLY blocks every INSERT, UPDATE and DELETE until the index is built. To accept it, put "-- migrator:lock-acknowledged share" at the head of the migration
exit 6
```

The refusal happens **under the advisory lock, before the first statement**, and
against the same statements that are about to be sent — so the plan cannot
change between the look and the leap.

`MIGRATOR_MAX_LOCK_LEVEL=share-update-exclusive` sets it once for a pipeline.
The accepted names are the PostgreSQL lock modes, lower case, hyphenated:
`access-share`, `row-share`, `row-exclusive`, `share-update-exclusive`, `share`,
`share-row-exclusive`, `exclusive`, `access-exclusive`.

## 3. Fix the migration

The refusal above is right: an ordinary `CREATE INDEX` blocks writes for as long
as the build takes. Rewrite it:

```sql
-- migrator:no-transaction

CREATE INDEX CONCURRENTLY users_email_idx ON users (email);
```

`CONCURRENTLY` cannot run inside a transaction, hence the directive. The plan
now says so:

```console
$ migrator up --dry-run
  Plan  2 migration(s) up

    20260901120000_add_email_index  no-transaction, 1 statement(s)
      CREATE INDEX CONCURRENTLY users_email_idx ON users (email)
        SHARE UPDATE EXCLUSIVE on users (~41 200 rows), scans the table
        CONCURRENTLY lets reads and writes continue, at the cost of two passes
        over the table and a failure mode that leaves an invalid index behind
```

## 4. Or accept it, in the file

Some changes genuinely need the heavy lock. The second migration is one: there
is no concurrent form of `ALTER COLUMN TYPE`.

```console
$ migrator up --max-lock-level share-update-exclusive
migrator: 20260901130000_widen_status.up.sql: statement 1 takes ACCESS EXCLUSIVE on orders (~8 900 rows), and the limit is SHARE UPDATE EXCLUSIVE; it rewrites the table. … To accept it, put "-- migrator:lock-acknowledged access-exclusive" at the head of the migration
exit 6
```

The acceptance goes **in the migration**, not on the command line:

```sql
-- migrator:lock-acknowledged access-exclusive

ALTER TABLE orders ALTER COLUMN status TYPE text;
```

```console
$ migrator up --max-lock-level share-update-exclusive
INF migrator: migration applied direction=up name=add_email_index took=23ms version=20260901120000
INF migrator: migration applied direction=up name=widen_status took=1ms version=20260901130000
  applied    20260901120000_add_email_index  23ms
  applied    20260901130000_widen_status  1ms

  Done. 2 applied in 38ms. Current version 20260901130000.
```

**There is no flag that waives the gate.** The decision belongs where the
knowledge is — at review time, in the file, where a reviewer can see it and
argue — not with whoever is holding the deploy at three in the morning and can
see only a flag that would make the refusal go away. The acknowledgement raises
the limit for one migration and only upwards; a file that acknowledges less than
it takes is still refused.

## What the prediction cannot see

It is a heuristic over the text of the statement, not a planner. It does not
know about:

- **the queue.** An `ALTER TABLE` behind a long-running `SELECT` blocks
  everything after it whatever lock it asked for. That is what `lock_timeout`
  is for — it defaults to 3s here, deliberately, because the alternative turns a
  slow migration into an outage;
- **triggers, rules and inheritance;**
- **the old column type.** `ALTER COLUMN TYPE` is always reported as rewriting,
  because the old type is not in the statement. `varchar(32) → text` does not
  actually rewrite. This is the one known source of a false alarm, and the
  reason is printed in the prediction so you can disagree with it.

Read it as a review that never gets tired, not as a guarantee. The rule table is
checked against a real PostgreSQL — an integration test runs each statement and
reads `pg_locks` to see what was actually taken — which is a different thing
from being checked against itself.

## Two timeouts worth setting

```sql
-- migrator:lock-timeout 5s
-- migrator:statement-timeout 30m
```

`lock_timeout` bounds how long the statement waits *for* the lock;
`statement_timeout` bounds how long it runs once it has it. Both can also be set
for the whole run with `--lock-timeout` and `--statement-timeout`, and the run
always sends them explicitly — a connection pool configured with
`statement_timeout=30s` would otherwise kill exactly the migrations this tool
exists to run.

## Next

- [Watching a long migration](watching-a-long-migration.md) — once it is
  running, and slow
- [In CI and in a deploy](ci-and-deploy.md)
