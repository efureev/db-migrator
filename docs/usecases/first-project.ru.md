# Новый проект с нуля

*[In English](first-project.md)*

**Когда нужно:** база пустая и этот инструмент прикасается к ней первым. Если
схема уже есть — вам в [Приём существующей базы](adopt-existing-database.ru.md):
`up` поверх уже существующих таблиц упадёт на первой же миграции.

## 1. Первая миграция

`create` не открывает базу вовсе. Пишет обе половины и печатает пути.

```console
$ migrator create add_users_table
migrations/20260819194642_add_users_table.up.sql
migrations/20260819194642_add_users_table.down.sql
```

Версия по умолчанию — метка времени (есть ещё `--format unix` и
`--format sequential`). Up-файл приходит с комментарием, который объясняет
директивы и намеренно ими не пользуется:

```sql
-- Write the migration below.
--
-- If it needs a directive, add one above the SQL, one per line, in the form
-- shown here with the leading marker restored:
--
--   ..migrator:no-transaction        required by CREATE INDEX CONCURRENTLY
--                                    and ALTER TYPE ... ADD VALUE
--   ..migrator:retry-safe            this migration is idempotent and may be
--                                    re-run after an interrupted attempt
--   ..migrator:statement-timeout 30m
--   ..migrator:lock-timeout 5s
--
-- Replace the ".." with "-- " when you use one. …
```

`..` — не опечатка. Настоящая строка `-- migrator:` в шаблоне была бы настоящей
директивой в каждом создаваемом файле.

Заполняем обе половины:

```sql
-- 20260819194642_add_users_table.up.sql
CREATE TABLE users (
    id    bigserial PRIMARY KEY,
    email text NOT NULL UNIQUE
);
```

```sql
-- 20260819194642_add_users_table.down.sql
DROP TABLE users;
```

## 2. Сначала посмотреть

`status` не пишет ничего — ни журнала, ни лока, — поэтому безопасен против
реплики и роли только на чтение.

```console
$ migrator status
  The journal "public.schema_migrations" does not exist yet: nothing has been applied.
  "migrator up" will create it.

  VERSION          NAME                         STATUS       APPLIED AT            TOOK
  20260819194642   add_users_table              pending      -                     -

  0 applied, 1 pending.  No current version.
```

## 3. Накатить

```console
$ migrator up
INF migrator: migration applied direction=up name=add_users_table took=2ms version=20260819194642
  applied    20260819194642_add_users_table  2ms

  Done. 1 applied in 21ms. Current version 20260819194642.
```

Два потока, и разделение существенно, если вы это скриптуете: ответ идёт в
**stdout**, комментарий по ходу — в **stderr**. `migrator up | tee deploy.log`
сохранит ответ и оставит комментарий на терминале; под `--json` stdout
становится одним JSON-объектом, а комментарий — строками JSON.

Миграция и строка, которая её записывает, живут в одной транзакции. «Применено,
но не записано» здесь невозможно — и именно поэтому миграция, которая **не может**
идти в транзакции, обязана сказать об этом директивой `-- migrator:no-transaction`;
такая записывается в два шага, чтобы падение между ними оставило след, а не
тишину.

## 4. Убедиться

```console
$ migrator status
  VERSION          NAME                         STATUS       APPLIED AT            TOOK
  20260819194642   add_users_table              applied      2026-08-19 22:46:57   2ms

  1 applied, 0 pending.  Current version 20260819194642.
```

Повторный `up` ничего не делает и возвращает 0. «Нечего делать» — это успех, а не
особый случай: деплою, который зовёт `migrator up` на каждом релизе, нужно именно
это.

```console
$ migrator up
Schema is up to date. Nothing to apply.
```

Для скрипта, которому нужна только версия:

```console
$ migrator status --current
20260819194642_add_users_table
```

## Как подключается

От высшего приоритета к низшему: явно набранный флаг, затем `MIGRATOR_*`, затем
`DATABASE_URL` и `PG*` из libpq, затем `--env-file`, затем `./.env`, затем
встроенное умолчание.

```bash
export MIGRATOR_DSN='postgres://app:secret@localhost:5432/shop?sslmode=disable'
migrator up
```

Пустой DSN — не ошибка: pgx тогда читает `PGHOST`, `PGUSER`, `PGDATABASE` и
`~/.pgpass`, поэтому ненастроенный `migrator` подключается ровно туда же, куда
`psql` без аргументов. `migrator config` покажет все разрешённые значения и
откуда каждое взялось.

## Дальше

- [Безопасное изменение схемы](safe-schema-change.ru.md) — что возьмёт каждый
  оператор, до того как он это возьмёт
- [В CI и в деплое](ci-and-deploy.ru.md) — коды возврата и `--json`
- [Откат и починка журнала](rollback-and-repair.ru.md)
