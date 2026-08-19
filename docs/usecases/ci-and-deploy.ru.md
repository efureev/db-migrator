# В CI и в деплое

*[In English](ci-and-deploy.md)*

**Когда нужно:** вы встраиваете инструмент в pipeline, где вывод никто не читает,
а весь разговор — это код возврата.

## Коды возврата

| Код | Значение | Правильная реакция |
|---|---|---|
| 0 | сделано или нечего делать | продолжать |
| 1 | прогон упал: неверный SQL, оборванное соединение | будить человека |
| 2 | команда не понята | править pipeline |
| 3 | есть непринятые миграции (`status --check`) | деплоить |
| 4 | файлы и журнал разошлись | нужен человек |
| 5 | лок держит другой migrator | повторить через минуту |
| 6 | отклонено защитой | править миграцию, а не базу |

Различие 5 и 1 — весь смысл таблицы: в CI правильная реакция на «лок держит
соседний деплой» — подождать минуту, а на «SQL неверен» — нет.

## Проверить файлы без базы

`validate --offline` разбирает файлы, проверяет имена, парность, директивы и те
чек-суммы, которые считаются без подключения. Ему место в той же джобе, что и
тестам:

```console
$ migrator validate --offline
  No problems found.
```

`--strict` поднимает предупреждения — например, отсутствующий down-файл — до
ошибок.

## Нужен ли деплой

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

`status` не берёт лока и ничего не создаёт, поэтому его безопасно гонять против
реплики и под ролью только на чтение.

```bash
if migrator status --check; then
    echo "нечего делать"
elif [ $? -eq 3 ]; then
    migrator up
else
    exit 1     # 4 — это дрейф: поверх него деплоить нельзя
fi
```

## Накатить

```console
$ migrator up
INF migrator: migration applied direction=up name=create_users took=1ms version=1
INF migrator: migration applied direction=up name=add_index took=0s version=2
  applied    1_create_users  1ms
  applied    2_add_index  0s

  Done. 2 applied in 15ms. Current version 2.
```

После этого `status --check` возвращает 0, а повторный `up` ничего не делает и
тоже возвращает 0. Pipeline, зовущий `migrator up` на каждом релизе, не обязан
знать, было ли что накатывать.

## Машиночитаемый вывод

Каждый объект верхнего уровня несёт `"format": 1`. Добавление поля версию не
меняет; удаление или смена смысла — меняет, и это отдельная строка в CHANGELOG.
Потребителю нужно сравнение, а не разбор.

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

**В stdout идёт ответ и ничего кроме.** Под `--json` комментарий в stderr тоже
становится строками JSON, поэтому потребитель, разбирающий stderr, не ломается на
первой же строке прогресса. `migrator status --json | jq -r .current_version`
безопасен; `migrator up --json 2>>deploy.log | jq .` тоже.

## Два деплоя одновременно

Прогоны сериализуются advisory-локом PostgreSQL, который берётся до создания
таблицы журнала. Второй прогон ждёт, а потом сдаётся с кодом, который говорит,
что повтор — правильный ход:

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

Сколько ждать, задаёт `--advisory-lock-timeout`, по умолчанию 30 секунд. Лок
сессионный, поэтому умерший процесс его отпускает — руками подчищать нечего.

**Пул в режиме transaction ломает это бесшумно**, поэтому инструмент его
обнаруживает и отказывается, а не притворяется. Миграции надо направлять в базу
напрямую или на порт с режимом session.

## В контейнере

```bash
docker run --rm \
  -e MIGRATOR_DSN='postgres://app:secret@db:5432/shop?sslmode=disable' \
  -e MIGRATOR_ENV=production \
  -v "$PWD/migrations:/migrations" \
  ghcr.io/efureev/migrator:2 up
```

`MIGRATOR_ENV=production` включает защиты из
[Защиты в продакшене](production-guards.ru.md): `down` и `wipe` становятся
недоступны, и флага, который это отменяет, нет.

Обратите внимание, у каких настроек намеренно **нет** переменной окружения:
`--allow-down`, `--allow-wipe` и `--confirm`. Переменная наследуется каждым
процессом в контейнере и каждым шагом CI-джобы, а флаг набирается для одного
запуска.

В Kubernetes это init-контейнер или Job: запустился, завершился, оркестратор
реагирует на код возврата. Демона и постоянно работающего процесса здесь нет по
замыслу.

## Джоба GitHub Actions

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

`--max-lock-level` — это то, что превращает dry-run из текста, который читает
человек, в правило, которое соблюдает pipeline; см.
[Безопасное изменение схемы](safe-schema-change.ru.md).

## Дальше

- [Защиты в продакшене](production-guards.ru.md)
- [Откат и починка журнала](rollback-and-repair.ru.md) — код 4 подробно
