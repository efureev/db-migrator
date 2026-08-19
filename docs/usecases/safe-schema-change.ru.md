# Безопасное изменение схемы

*[In English](safe-schema-change.md)*

**Когда нужно:** вы собираетесь изменить таблицу, из которой прямо сейчас
читают, и хотите знать, что оператор заблокирует, до того как он это сделает.

Это то, ради чего инструмент существует. Не «эта миграция рискованная» — а
уровень блокировки, таблица, сколько в ней примерно строк и переписывает она её
или сканирует.

## 1. Спросить до того, как запускать

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

Число строк берётся из `pg_class.reltuples` — это оценка, и её нет вовсе, пока
таблицу кто-нибудь не `ANALYZE`. Разряды сгруппированы, потому что разница между
8 900 000 и 890 000 — это разница между предупреждением, которое стоит услышать,
и тем, которое можно пропустить, а разряды никто не считает.

`--dry-run --json` даёт тот же план для машины.

## 2. Превратить знание в правило

Читать план каждый раз работает ровно до того дня, когда кто-нибудь не прочитает.
Поставьте потолок и позвольте инструменту отказать:

```console
$ migrator up --max-lock-level share-update-exclusive
migrator: 20260901120000_add_email_index.up.sql: statement 1 takes SHARE on users (~41 200 rows), and the limit is SHARE UPDATE EXCLUSIVE; it scans the table. CREATE INDEX without CONCURRENTLY blocks every INSERT, UPDATE and DELETE until the index is built. To accept it, put "-- migrator:lock-acknowledged share" at the head of the migration
exit 6
```

Отказ происходит **под advisory-локом, до первого оператора**, и против ровно тех
операторов, которые сейчас будут отправлены, — план не может измениться между
«посмотрели» и «прыгнули».

`MIGRATOR_MAX_LOCK_LEVEL=share-update-exclusive` ставит это один раз на весь
pipeline. Принимаемые имена — режимы блокировок PostgreSQL в нижнем регистре
через дефис: `access-share`, `row-share`, `row-exclusive`,
`share-update-exclusive`, `share`, `share-row-exclusive`, `exclusive`,
`access-exclusive`.

## 3. Починить миграцию

Отказ выше справедлив: обычный `CREATE INDEX` блокирует запись на всё время
сборки. Переписываем:

```sql
-- migrator:no-transaction

CREATE INDEX CONCURRENTLY users_email_idx ON users (email);
```

`CONCURRENTLY` не может идти внутри транзакции — отсюда директива. План теперь
говорит это:

```console
$ migrator up --dry-run
  Plan  2 migration(s) up

    20260901120000_add_email_index  no-transaction, 1 statement(s)
      CREATE INDEX CONCURRENTLY users_email_idx ON users (email)
        SHARE UPDATE EXCLUSIVE on users (~41 200 rows), scans the table
        CONCURRENTLY lets reads and writes continue, at the cost of two passes
        over the table and a failure mode that leaves an invalid index behind
```

## 4. Или согласиться — в файле

Некоторые изменения действительно требуют тяжёлой блокировки. Вторая миграция
именно такая: у `ALTER COLUMN TYPE` нет конкурентной формы.

```console
$ migrator up --max-lock-level share-update-exclusive
migrator: 20260901130000_widen_status.up.sql: statement 1 takes ACCESS EXCLUSIVE on orders (~8 900 rows), and the limit is SHARE UPDATE EXCLUSIVE; it rewrites the table. … To accept it, put "-- migrator:lock-acknowledged access-exclusive" at the head of the migration
exit 6
```

Согласие ставится **в миграции**, а не в командной строке:

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

**Флага, снимающего эти ворота, нет.** Решение принадлежит тому месту, где есть
знание, — ревью, файлу, где рецензент его видит и может возразить, — а не тому,
кто держит деплой в три часа ночи и видит только флаг, от которого отказ исчезнет.
Согласие поднимает предел для одной миграции и только вверх; файл, признавший
меньше, чем берёт, всё равно получит отказ.

## Чего прогноз не видит

Это эвристика по тексту оператора, а не планировщик. Она не знает про:

- **очередь.** `ALTER TABLE` за длинным `SELECT`ом блокирует всё последующее,
  какой бы лок он ни просил. За это отвечает `lock_timeout` — здесь он по
  умолчанию 3 секунды, намеренно, потому что иначе медленная миграция
  превращается в отказ сервиса;
- **триггеры, правила и наследование;**
- **старый тип колонки.** `ALTER COLUMN TYPE` **всегда** считается переписывающим,
  потому что старого типа в операторе нет. `varchar(32) → text` на самом деле не
  переписывает. Это единственный известный источник ложной тревоги, и причина
  печатается прямо в прогнозе, чтобы с ней можно было не согласиться.

Читайте это как ревью, которое не устаёт, а не как гарантию. Таблица правил
сверяется с настоящим PostgreSQL: интеграционный тест запускает каждый оператор
и читает `pg_locks`, чтобы увидеть, что сервер взял на самом деле, — это другое,
чем проверять таблицу против самой себя.

## Два таймаута, которые стоит ставить

```sql
-- migrator:lock-timeout 5s
-- migrator:statement-timeout 30m
```

`lock_timeout` ограничивает, сколько оператор ждёт лок; `statement_timeout` —
сколько он работает, получив его. Оба можно задать и на весь прогон
(`--lock-timeout`, `--statement-timeout`), и прогон **всегда** отправляет их
явно: пул, настроенный с `statement_timeout=30s`, иначе убил бы ровно те
миграции, ради которых этот инструмент существует.

## Дальше

- [Наблюдение за долгой миграцией](watching-a-long-migration.ru.md) — когда она
  уже идёт и идёт долго
- [В CI и в деплое](ci-and-deploy.ru.md)
