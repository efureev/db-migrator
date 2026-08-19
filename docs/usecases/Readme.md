# Use cases

*[По-русски](Readme.ru.md)*

Each page is one task, walked end to end. The commands are real and so is their
output: every transcript here was produced by running the tool against a live
PostgreSQL, not written by hand.

## Start here

| I want to… | Page |
|---|---|
| set this up on an empty database | [Starting a new project](first-project.md) |
| point it at a database that already has a schema | [Adopting an existing database](adopt-existing-database.md) |

## Day to day

| I want to… | Page |
|---|---|
| know what a migration will lock before it locks it | [Making a schema change safely](safe-schema-change.md) |
| find out whether the migration that has been running for 20 minutes is stuck | [Watching a long migration](watching-a-long-migration.md) |
| wire it into CI and a deploy | [In CI and in a deploy](ci-and-deploy.md) |
| undo something, or fix a journal that disagrees with the files | [Rolling back, and repairing the journal](rollback-and-repair.md) |

## Setting it up

| I want to… | Page |
|---|---|
| decide what it may do against a real database | [Guards in production](production-guards.md) |
| run one set of files against many schemas | [One set of migrations, many schemas](many-schemas.md) |
| have a service apply its own migrations at startup | [Using it as a library](library.md) |

## By exit code

The exit code is the whole conversation in a pipeline:

| Code | Meaning | Start at |
|---|---|---|
| 3 | migrations are pending | [In CI and in a deploy](ci-and-deploy.md) |
| 4 | files and journal disagree | [Rolling back, and repairing the journal](rollback-and-repair.md) |
| 5 | another migrator holds the lock | [In CI and in a deploy](ci-and-deploy.md) |
| 6 | refused by a guard | [Guards in production](production-guards.md) or [Making a schema change safely](safe-schema-change.md) |

## Elsewhere

- [Readme](../../Readme.md) — what the tool is, the full settings table, every
  command
- [UPGRADE.md](../../UPGRADE.md) — coming from version 1
- [CHANGELOG.md](../../CHANGELOG.md) — what changed in each release, and why
