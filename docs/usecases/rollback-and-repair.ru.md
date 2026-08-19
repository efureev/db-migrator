# Откат и починка журнала

*[In English](rollback-and-repair.md)*

**Когда нужно:** уехало то, что уезжать не должно было, — или журнал и файлы
разошлись, и `up` остановился с кодом 4.

Два разных инструмента на две разные задачи, и различие существенно: `down`
меняет схему, `repair` меняет только журнал и схему не трогает никогда.

## Откат

```console
$ migrator down --allow-down --yes
INF migrator: migration applied direction=down name=add_index took=2ms version=2
  reverted   2_add_index  2ms

  Done. 1 rolled back in 25ms.
```

`--allow-down` обязателен и намеренно не имеет переменной окружения: он говорит,
что вы имели это в виду — сейчас. В production-окружении `down` отвергается
целиком, и флага, который это отменяет, нет; см.
[Защиты в продакшене](production-guards.ru.md).

Строка обновляется, а не удаляется. История того, что выполнялось, дороже
опрятной таблицы:

```console
$ migrator status
  VERSION          NAME                         STATUS       APPLIED AT            TOOK
  1                create_users                 applied      2026-08-19 22:52:58   1ms
  2                add_index                    rolled back  2026-08-19 22:52:58   2ms

  1 applied, 1 pending.  Current version 1.
```

`--steps n` откатывает n; `--to <версия>` откатывает всё выше неё, оставляя её в
силе; `--dry-run` печатает план и не меняет ничего.

## Откатить и накатить заново

`redo` делает и то и другое под одним локом — это развойная половина того, чем
раньше была `fresh`:

```console
$ migrator redo --allow-down --yes
  reverted   1_create_users  3ms
  applied    1_create_users  2ms

  Done. 2 rolled back in 21ms. Current version 1.
```

Обе половины проверяются против `--max-lock-level` **до** того, как выполнится
хоть одна. Отказ на пути вверх, когда откат уже произошёл, оставил бы базу там,
куда никто не просил.

Разрушительная половина — `migrator wipe && migrator up`, и это две команды
намеренно.

## Когда файл изменили после применения

Каждая миграция несёт чек-сумму своего текста. Правка уже применённого файла
отвергается — громко и до того, как что-либо запустится, — потому что каждая база,
применившая старый текст, нового уже не увидит.

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

`validate` показывает всю картину разом, ошибки и предупреждения вместе:

```console
$ migrator validate
  1_create_users.up.sql: error: applied as a4dae7e71899, on disk 6d90659849b0
  2_add_index.up.sql: warning: not applied

  1 error(s), 1 warning(s).
exit 4
```

Выходов два, и первый почти всегда правильный:

1. **Вернуть файл и написать новую миграцию.** В базе лежит то, что произвёл
   старый текст; новый текст не выполнялся нигде.
2. **Принять новую чек-сумму**, если правка действительно не изменила ничего
   выполняемого — комментарий, пробелы, переименование в комментарии. (Переводы
   строк сами по себе учтены: чек-сумма считается по нормализованной форме,
   поэтому чекаут под Windows с `core.autocrlf=true` не выглядит правкой.)

```console
$ migrator repair --rehash 1
INF migrator: repaired operations=1
  rehash 1: a4dae7e71899 -> 6d90659849b0
```

**У `up` нет `--force-checksum`**, и это осознанно. За флагом на команде,
накатывающей миграции, тянутся в спешке — ровно в тот момент, когда громкий отказ
делает свою работу. `repair` называет версию поимённо, печатает обе суммы и не
трогает ничего больше.

## Когда миграция началась и не вернулась

Оставить такое может только не-транзакционный путь: строка фиксируется до
выполнения операторов и обновляется после, поэтому падение между ними видно потом,
а не пропадает в тишине.

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

Ничего не запустится, пока это не разрешит человек:

```console
$ migrator up
migrator: migration recorded as started was never confirmed: 2_add_index started at 2026-08-19T22:55:42+03:00 by fureev@MacBook-Pro-Evgenij.local
exit 4
```

**Автоповтор здесь был бы неверен.** Упавший `CREATE INDEX CONCURRENTLY`
оставляет невалидный индекс, который повторный `CONCURRENTLY` не заменит — упадёт
с 42P07. Поэтому сначала смотрим схему:

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

Индекс на месте и валиден, значит работа сделана и потерян только учёт:

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

Если бы схема показала, что миграция **не** подействовала, второй выход —
`repair --discard 2`: он забывает версию, и она выполняется заново.

```console
$ migrator repair --discard 2
INF migrator: repaired operations=1
  discard 2
```

Миграция, которая действительно идемпотентна, может сказать это и пропустить весь
разговор:

```sql
-- migrator:no-transaction
-- migrator:retry-safe
```

## Все виды починки

| Флаг | Что делает |
|---|---|
| `--rehash <v>` | записать текущую чек-сумму файла для версии `v` |
| `--complete <v>` | пометить прерванную не-транзакционную миграцию завершённой |
| `--discard <v>` | забыть версию `v`, чтобы она выполнилась заново |
| `--prune` | забыть все записанные версии, у которых пропал файл |

`repair` не выполняет SQL миграций и не трогает схему, поэтому доступен в любом
окружении — включая то, где нужен больше всего.

## Дальше

- [Защиты в продакшене](production-guards.ru.md)
- [Наблюдение за долгой миграцией](watching-a-long-migration.ru.md) — как увидеть
  ту, которую вот-вот прервут
