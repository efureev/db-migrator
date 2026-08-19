# Как библиотека

*[In English](library.md)*

**Когда нужно:** сервис применяет свои миграции сам, при старте, а файлы едут
внутри бинарника.

Вся модель — три значения. **Источник** — набор файлов миграций, прочитанный
через `io/fs.FS`, поэтому подходят и `os.DirFS`, и `embed.FS`, и `fstest.MapFS`, а
разбирающая половина тестируется вообще без базы. **План** говорит, что прогон
сделал бы. **Отчёт** говорит, что прогон сделал.

## Как это выглядит

Работающая версия лежит в [`examples/embed`](../../examples/embed).

```go
//go:embed migrations/*.sql
var embedded embed.FS

func migrate(ctx context.Context, log *slog.Logger) error {
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()

	// Встроенная FS укоренена выше файлов; fs.Sub указывает на каталог с ними,
	// а это и есть то, чего ждёт Load.
	src, err := fs.Sub(embedded, "migrations")
	if err != nil {
		return err
	}

	// Ни WithAllowDown, ни WithAllowWipe: сервису нечего откатывать и стирать
	// собственную схему, и отсутствие этих опций делает такое невозможным, а не
	// просто не рекомендованным.
	m, err := migrator.New(migrator.FromPool(pool), src,
		migrator.WithLogger(log),
		migrator.WithMigratorTag("shop-api/1.4.2"),
	)
	if err != nil {
		return err
	}

	report, err := m.Up(ctx)
	if err != nil {
		return err
	}

	log.Info("migrations applied", "report", report)

	return nil
}
```

```console
$ DATABASE_URL=... go run ./examples/embed
level=INFO msg="migrator: migration applied" version=1 name=create_users direction=up took=4ms
level=INFO msg="migrator: migration applied" version=2 name=index_users_email direction=up took=3ms
level=INFO msg="migrations applied" report.direction=up report.applied=2 report.duration=46.575458ms report.current=2
```

Второй запуск не делает ничего и молчит:

```console
$ DATABASE_URL=... go run ./examples/embed
level=INFO msg="migrations applied" report.direction=up report.applied=0 report.duration=29.087417ms report.current=0
```

`Report` реализует `slog.LogValuer` — поэтому весь прогон уезжает в лог одним
сгруппированным полем.

## Почему Connector, а не пул

`New` принимает `Connector`, который выдаёт **одно закреплённое соединение
PostgreSQL на весь прогон**. Это не оптимизация.

Advisory-лок, сериализующий одновременные прогоны, сессионный, и `SET` тоже
действует на одно соединение. Взятый на одном и отпущенный на другом, лок не
отпущен вовсе, а таймауты, нужные миграции, не доезжают до оператора, которому они
были нужны. `*pgxpool.Pool` удовлетворяет `Exec` и `Begin` и скомпилировался бы —
и сломался бы один прогон из ста, под нагрузкой, невоспроизводимо.

Отсюда три конструктора, явных на этот счёт:

| Конструктор | Что делает | Прогресс |
|---|---|---|
| `FromDSN(dsn)` | открывает соединение на прогон | включён по умолчанию |
| `FromPool(pool)` | вынимает одно соединение и держит | включён по умолчанию |
| `FromConn(conn)` | пользуется вашим соединением | недоступен, см. ниже |

`FromConn` возвращает соединение, которым владеет вызывающий, а оно занято
выполнением миграции — опрашивать `pg_stat_progress_*` неоткуда. Это
обнаруживается, а не предполагается, и говорится один раз на уровне debug. См.
[Наблюдение за долгой миграцией](watching-a-long-migration.ru.md).

`Conn` экспортирован затем, чтобы соединение можно было обернуть — ради
трассировки, ради метрик — и всё ещё удовлетворять `Connector`. Это законное
желание, поэтому тип остался публичным, когда остальную поверхность сокращали.

## Дерево зависимостей

**Библиотека зависит от `jackc/pgx/v5` и больше ни от чего.** Ни от логгера, ни
от библиотеки конфигурации, ни от CLI-фреймворка. Каждая транзитивная зависимость
мигратора становится транзитивной зависимостью чужого продакшн-бинарника — ровно
поэтому правило проверяет линтер, а не ревью.

Логирование — `log/slog` из стандартной библиотеки, и логгер по умолчанию
выбрасывает всё: библиотека, которая пишет туда, куда её не просили, — это
библиотека, портящая кому-то JSON в stdout.

## Опции, которые стоит знать

```go
m, err := migrator.New(migrator.FromPool(pool), src,
	migrator.WithSchema("app"),              // журнал здесь, и засевает @schema@
	migrator.WithTable("schema_migrations"),
	migrator.WithLogger(log),
	migrator.WithMigratorTag("shop-api/1.4.2"), // пишется в журнал
	migrator.WithAppliedBy("deploy-bot"),       // пишется в журнал
	migrator.WithStatementTimeout(30*time.Minute),
	migrator.WithLockTimeout(5*time.Second),
	migrator.WithMaxLockLevel(migrator.ShareUpdateExclusive),
	migrator.WithProgressInterval(15*time.Second),
	migrator.WithPlaceholders(map[string]string{"@tablespace@": "fast_ssd"}),
)
```

`Option` — запечатанный интерфейс, его единственный метод неэкспортирован,
поэтому структура конфигурации остаётся целиком внутренней и, следовательно,
свободной для изменений. `type Option func(*Config)` с экспортированным `Config`
заморозил бы её под SemVer в первый же день.

Намеренно отсутствуют, пока не попросите: `WithAllowDown`, `WithAllowWipe`,
`WithForceWipe`. Не давая их сервису, вы делаете откат невозможным, а не просто
не рекомендованным.

## Решить, не делая

```go
plan, err := m.Plan(ctx, migrator.DirectionUp, migrator.All())
```

`Plan` открывает собственную сессию, не берёт лока и ничего не пишет. Каждый
`Step` несёт операторы в том виде, в каком они будут отправлены, и
`LockPrediction` на оператор: какой лок, переписывает ли, сканирует ли, сколько
строк в таблице. Рендерят `Plan.Text(w)` и `Plan.JSON(w)`. См.
[Безопасное изменение схемы](safe-schema-change.ru.md).

`Status` и `Version` read-only таким же образом, и ни один не требует права на
создание — поэтому оба остаются доступны против реплики и под ролью только на
чтение.

## Ошибки — сентинелы

Сопоставлять через `errors.Is`, никогда по тексту:

```go
switch {
case errors.Is(err, migrator.ErrLockTimeout):
	// мигрирует соседний деплой; повтор — правильный ход
case errors.Is(err, migrator.ErrChecksumMismatch):
	// выпущенный файл отредактировали; нужен человек
case errors.Is(err, migrator.ErrForeignJournal):
	// мешает чужой журнал — см. Adopt
}
```

За подробностями — `errors.As` в типизированные: `*MigrationError` несёт версию,
файл, номер оператора и строку; `*LockLevelError` — отвергнутый прогноз.

Всё, что говорит сервер, проходит через редактирование до того, как попадёт в
лог, — DSN с паролем не уедет в чужой сборщик логов.

## Как тестировать свои миграции

Разбирающая половина не нуждается в базе:

```go
src := fstest.MapFS{
	"1_create_users.up.sql": &fstest.MapFile{Data: []byte("CREATE TABLE users (id int);")},
}

m, err := migrator.New(conn, src)   // Load разбирает и проверяет здесь
```

Для половины, которой база нужна, давайте каждому тесту **свою базу**, а не свою
схему, и помните, что advisory-локи общекластерные: два теста с умолчательным
`public.schema_migrations` выводят один и тот же ключ лока и будут сериализоваться
друг с другом, хотели вы того или нет.

## Дальше

- [Один набор миграций на много схем](many-schemas.ru.md)
- [Наблюдение за долгой миграцией](watching-a-long-migration.ru.md)
