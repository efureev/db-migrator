# Приём существующей базы

*[In English](adopt-existing-database.md)*

**Когда нужно:** схема уже есть — построена руками, golang-migrate или первой
версией этого инструмента, — и вы хотите передать её сюда, ничего не перезапуская.

Без этого `up` попытался бы накатить первую миграцию поверх уже существующих
таблиц. Инструмент это замечает и отказывается, а не пробует:

```console
$ migrator up
migrator: the journal table belongs to another tool: "public"."schema_migrations" exists and has no "checksum" column, so it is not this tool's journal (golang-migrate's is version+dirty) — see "migrator adopt"
exit 6
```

Код 6 — «отклонено защитой»: повторять бессмысленно, и чинить надо не базу.

## Единственный риск, названный прямо

`adopt` пишет журнал, который **был бы**, если бы схему строил этот инструмент,
и не выполняет ни одного оператора миграции. Он верит вам на слово, что файлы
соответствуют тому, что лежит в базе. Это слово и есть весь риск.

Поэтому сначала проверьте, по возможности на копии:

```bash
pg_dump --schema-only "$PRODUCTION" > before.sql
# накатите файлы на пустую базу, снимите дамп, сравните
```

И строки помечаются навсегда. `status` показывает их как `(adopted)`, потому что
«применено» и «нам сказали, что применено» — разные утверждения, и здесь
наблюдали только второе.

## Из журнала golang-migrate

golang-migrate держит `schema_migrations (version bigint, dirty boolean)`.
Укажите на него, и версия прочитается сама.

Сначала вхолостую — перечисляет, что будет записано, и не пишет ничего:

```console
$ migrator adopt --from-golang-migrate --dry-run
  adopted    1_create_users  0s
  adopted    2_create_orders  0s

  Dry run: 2 row(s) would be written, nothing was. Current version 2.
```

Затем по-настоящему:

```console
$ migrator adopt --from-golang-migrate
INF migrator: moved the old journal aside from=schema_migrations to=schema_migrations_pre_v2
INF migrator: adopted baseline=2 database=uc_adopt recorded=2
  adopted    1_create_users  0s
  adopted    2_create_orders  0s

  Done. 2 applied in 21ms. Current version 2.
```

Старый журнал **переименован, а не удалён**: `schema_migrations_pre_v2`.
Возможность вернуться к прежнему инструменту должна оставаться ещё сутки.
Удалите его сами, позже, когда будете уверены.

```console
$ migrator status
  VERSION          NAME                         STATUS       APPLIED AT            TOOK
  1                create_users (adopted)       applied      2026-08-19 22:48:39   -
  2                create_orders (adopted)      applied      2026-08-19 22:48:40   -
  3                add_orders_total             pending      -                     -

  2 applied, 1 pending.  Current version 2.
```

Накатывается только то, что действительно ожидало:

```console
$ migrator up
INF migrator: migration applied direction=up name=add_orders_total took=1ms version=3
  applied    3_add_orders_total  1ms

  Done. 1 applied in 12ms. Current version 3.
```

### Журнал, помеченный dirty, отвергается

Так golang-migrate помечает миграцию, которая началась и не закончилась:

```console
$ migrator adopt --from-golang-migrate
migrator: the existing journal is marked dirty: version 2 is marked dirty, so what it left behind is unknown — resolve it with the old tool first
exit 6
```

Не записано ничего. Принять базу в таком состоянии значит зафиксировать
неизвестное состояние схемы как правильное — разберитесь тем инструментом,
который его оставил, и возвращайтесь.

## Из схемы, построенной руками

Когда журнала нет вовсе, скажите, до какого места база доехала:

```console
$ migrator adopt --baseline 2 --confirm shop
INF migrator: adopted baseline=2 database=shop recorded=2
  adopted    1_create_users  0s
  adopted    2_create_orders  0s

  Done. 2 applied in 12ms. Current version 2.
```

Всё до baseline включительно записывается применённым, всё выше остаётся
ожидающим.

`--confirm <база>` обязателен вне development. `adopt` не трогает схему, но
записывает в базу утверждение, которое потом никто не перепроверит, — и опечатка
в имени базы это то, что спасёт вас от записи его не туда.

## Флаги, которые стоит знать

| Флаг | Зачем |
|---|---|
| `--baseline <v>` | записать все миграции до `v` включительно |
| `--from-golang-migrate` | прочитать версию из журнала `(version, dirty)` и отодвинуть эту таблицу |
| `--dry-run` | перечислить, что было бы записано, и не записывать |
| `--confirm <база>` | имя базы; обязательно вне development |
| `--force` | принять поверх журнала, в котором уже есть строки |

`--force` существует для случая, когда предыдущая попытка дошла до середины. Без
него непустой журнал — отказ: приём это операция для базы, которая под этот
инструмент ещё не попадала.

## Дальше

- [В CI и в деплое](ci-and-deploy.ru.md)
- [Безопасное изменение схемы](safe-schema-change.ru.md)
