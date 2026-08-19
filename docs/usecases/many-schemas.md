# One set of migrations, many schemas

*[По-русски](many-schemas.ru.md)*

**Use this when** the same schema shape is deployed per tenant, per environment
or per test — one directory of files, several schemas in one database.

## The placeholder

`--schema` puts the journal in that schema and, at the same time, seeds the
`@schema@` placeholder, so a migration written for a configurable schema needs no
further wiring:

```sql
-- 1_create_invoices.up.sql
CREATE TABLE @schema@.invoices (
    id     bigserial PRIMARY KEY,
    amount numeric(12,2) NOT NULL
);
```

```console
$ migrator up --schema tenant_a
INF migrator: migration applied direction=up name=create_invoices took=1ms version=1
  applied    1_create_invoices  1ms

  Done. 1 applied in 16ms. Current version 1.

$ migrator up --schema tenant_b
INF migrator: migration applied direction=up name=create_invoices took=1ms version=1
  applied    1_create_invoices  1ms

  Done. 1 applied in 12ms. Current version 1.
```

The schema is created if it is not there. Each tenant gets its own journal:

```console
$ psql -c '\dt tenant_a.*' -c '\dt tenant_b.*'
  Schema  |       Name        | Type  |  Owner
----------+-------------------+-------+----------
 tenant_a | invoices          | table | migrator
 tenant_a | schema_migrations | table | migrator

  Schema  |       Name        | Type  |  Owner
----------+-------------------+-------+----------
 tenant_b | invoices          | table | migrator
 tenant_b | schema_migrations | table | migrator
```

```console
$ migrator status --schema tenant_a
  VERSION          NAME                         STATUS       APPLIED AT            TOOK
  1                create_invoices              applied      2026-08-19 22:56:08   1ms

  1 applied, 0 pending.  Current version 1.
```

## Tenants do not queue behind each other

The advisory lock the tool serialises on is derived from the schema and table
names, so each schema has a lock of its own:

```console
$ migrator locks --schema tenant_a
  Lock     1835496052/1363501398  (derived from tenant_a.schema_migrations)
  Nobody is holding it.

$ migrator locks --schema tenant_b
  Lock     1835496052/998994211  (derived from tenant_b.schema_migrations)
  Nobody is holding it.
```

Two tenants can migrate at the same time. Two runs against the *same* tenant
cannot, which is the point.

The derivation is part of the contract: changing it in a minor version would let
two versions of the tool migrate the same schema at the same time. If you need a
lock of your own — because two different tools share a schema, say — set it with
`WithLockID`.

## Rules and placeholders

Substitution is **textual and unconditional**. It does not know SQL, so:

- a placeholder may stand only where an identifier stands. `@schema@` inside a
  string literal is substituted too;
- a token of the form `@name@` left unresolved after substitution is an error.
  A mistyped `@tabel@` must not reach PostgreSQL as a syntax error halfway
  through a DDL script;
- **the checksum is taken over the file before substitution**, so pointing two
  deployments at two schemas is not mistaken for somebody having edited a
  released file.

Your own placeholders come from `WithPlaceholders`, and the key is written with
its delimiters:

```go
migrator.WithPlaceholders(map[string]string{"@tablespace@": "fast_ssd"})
```

A bare `tenant` as a key would be a blind substring replacement over the whole
file — rewriting `tenants` and `tenant_id` too — and the unresolved-token check
could not notice, because there would be no token left to find. Such a key is
rejected when the Migrator is built.

## Driving many tenants

There is no `--tenants` flag today. From a shell:

```bash
psql -Atc "SELECT nspname FROM pg_namespace WHERE nspname LIKE 'tenant_%'" |
while read -r schema; do
    migrator up --schema "$schema" || exit 1
done
```

Two things are worth deciding before you write that loop, because they are the
whole difficulty:

- **how many at a time.** Five hundred tenants is five hundred connections if
  nothing limits it. Sequential is slow and predictable;
- **what happens on the first failure.** Stopping leaves the earlier tenants
  migrated and the rest not; carrying on leaves you needing to know *which* ones
  failed. "12 of 500 failed" is useless if it does not say which.

From Go there is no per-tenant helper either. A `Migrator` is cheap and
immutable, so build one per schema:

```go
for _, schema := range tenants {
	m, err := migrator.New(migrator.FromPool(pool), src,
		migrator.WithSchema(schema),
		migrator.WithLogger(log.With("tenant", schema)),
	)
	if err != nil {
		return err
	}

	if _, err := m.Up(ctx); err != nil {
		return fmt.Errorf("tenant %s: %w", schema, err)
	}
}
```

Note `FromPool` here: each `Up` takes one connection out of the pool and holds it
for the whole of that tenant's run.

## Next

- [Using it as a library](library.md)
- [In CI and in a deploy](ci-and-deploy.md)
