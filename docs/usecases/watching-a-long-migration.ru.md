# Наблюдение за долгой миграцией

*[In English](watching-a-long-migration.md)*

**Когда нужно:** миграция идёт двадцать минут, и единственный честный ответ на
вопрос «она зависла?» сейчас — «не знаю».

PostgreSQL сам сообщает прогресс своих долгих операций, и почти никто этим не
пользуется. Раз в `--progress-interval` — по умолчанию 30 секунд — второе
соединение читает эти вью по бэкенду, выполняющему миграцию, и пишет строку.

## Сборка индекса

```console
$ migrator up --progress-interval 3s
INF migrator: migration in progress command=CREATE INDEX CONCURRENTLY direction=up elapsed=3s name=add_email_idx object=users_email_idx percent=32 phase=building index: scanning table pid=175181 progress=15 of 46 blocks relation=users source=create_index version=20260901120000
INF migrator: migration in progress command=CREATE INDEX CONCURRENTLY direction=up elapsed=6s name=add_email_idx object=users_email_idx percent=67 phase=building index: scanning table pid=175181 progress=31 of 46 blocks relation=users source=create_index version=20260901120000
INF migrator: migration applied direction=up name=add_email_idx took=8.762s version=20260901120000
  applied    20260901120000_add_email_idx  8.762s

  Done. 1 applied in 9.054s. Current version 20260901120000.
```

`pid` здесь потому, что его и передают в `pg_terminate_backend`. `percent` —
числом, чтобы на него можно было повесить алерт. `elapsed` меряется на этой
стороне и говорит, сколько применяется **эта миграция**.

По одной вью есть у `CREATE INDEX`, `VACUUM`, `ANALYZE`, `CLUSTER` и `COPY`.

## Всё остальное: пульс

Переписывание таблицы, бэкфилл-`UPDATE`, ожидание на локе — ничего из этого не
видно ни одной progress-вью, а вместе они составляют большинство долгих миграций.
Поэтому `pg_stat_activity` читается на каждом опросе тоже, и когда progress-строки
нет, сообщается он:

```console
$ migrator up --progress-interval 2s
INF migrator: migration still running direction=up elapsed=2s name=backfill_tz pid=175285 state=active statement_age=2s version=20260901140000 wait_event=Timeout:PgSleep
INF migrator: migration still running direction=up elapsed=4s name=backfill_tz pid=175285 state=active statement_age=4s version=20260901140000 wait_event=Timeout:PgSleep
INF migrator: migration still running direction=up elapsed=6s name=backfill_tz pid=175285 state=active statement_age=6s version=20260901140000 wait_event=Timeout:PgSleep
INF migrator: migration applied direction=up name=backfill_tz took=11.94s version=20260901140000
  applied    20260901140000_backfill_tz  11.94s

  Done. 1 applied in 11.966s. Current version 20260901140000.
```

Два разных сообщения, а не одно с полем-дискриминатором: потребителю, который
фильтрует JSON по `msg`, «индекс собран на 30%» и «жив, но четырнадцать минут
молчит» — разные события.

Смотреть надо на `wait_event`. `Timeout:PgSleep` выше — это спит фикстура;
`Lock:relation` означает, что миграция вовсе не долгая, а заблокирована — см.
[Безопасное изменение схемы](safe-schema-change.ru.md) и `lock_timeout`.
`statement_age` приходит с сервера и говорит, сколько идёт **текущий оператор**;
на не-транзакционной миграции из нескольких операторов он расходится с `elapsed`,
поэтому это два разных поля.

## Кто держит

Из другого терминала, пока прогон идёт:

```console
$ migrator locks
  Lock     1835496052/142344567  (derived from public.schema_migrations)

  pid 175285  migrator  on uc_pulse  active for 6s
    UPDATE users SET tz = slow(email) WHERE tz IS NULL;

  To end one:  SELECT pg_terminate_backend(175285);
```

`locks` не берёт лок и ничего не пишет. Держатели намеренно **не** фильтруются по
базе: advisory-локи общекластерные, и держатель, подключённый к соседней базе того
же сервера, — ровно тот случай, на объяснение которого иначе уходит двадцать минут.

## Как выключить и сколько это стоит

```bash
migrator up --progress-interval 0        # никогда
migrator up --progress-interval 5s       # чаще
export MIGRATOR_PROGRESS_INTERVAL=10s    # один раз на pipeline
```

Значения ниже 100 мс поднимаются до 100 мс. Строки идут в **stderr** и под
`--json` становятся JSON вместе со всем остальным, поэтому потребитель, который
льёт stdout в `jq`, их не увидит. `--quiet` и `--log-level error` их гасят —
это уровень `INFO`.

Отчётность стоит одного лишнего соединения на время прогона, и оно не
открывается вовсе, когда было бы бессмысленным:

- `--progress-interval 0`;
- логгер не задан (в библиотечном сценарии) — строки некому читать;
- коннектор не может дать второе соединение.

## В библиотеке

```go
m, err := migrator.New(migrator.FromPool(pool), src,
	migrator.WithLogger(log),
	migrator.WithProgressInterval(15*time.Second),
)
```

`FromDSN` и `FromPool` дают второе соединение сами, и отчётность включена по
умолчанию. `FromConn` возвращает то соединение, которым владеет вызывающий, а оно
занято выполнением миграции — опрашивать не с чего.

Это **обнаруживается, а не предполагается**: оба соединения спрашиваются про
`pg_backend_pid()` до первого оператора, и если ответ называет один бэкенд,
отчётность выключается одной строкой на уровне debug. Чужая обёртка вокруг одного
соединения ловится так же.

Чтобы опрашивать через своё соединение — реплику, другую роль, отдельный пул, —
передайте его:

```go
migrator.WithProgress(migrator.FromDSN(monitorDSN))
```

Этой роли нужно уметь видеть бэкенд: та же роль, что у прогона, либо роль с
`pg_read_all_stats`. Если не может — `pg_stat_activity` вернёт строку, у которой
все интересные колонки пусты, и отчётность выключит себя со строкой об этом, а не
будет вечно печатать пустоту.

Ничто в прогрессе не может уронить прогон. Пул с одним соединением, сервер,
который не отвечает, роль, которой не положено смотреть, — каждое из этого значит
«без отчётности» и одну строку на уровне debug.

## Дальше

- [Откат и починка журнала](rollback-and-repair.ru.md) — когда долгая умерла
  посередине
- [Безопасное изменение схемы](safe-schema-change.ru.md)
