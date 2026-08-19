# Один набор миграций на много схем

*[In English](many-schemas.md)*

**Когда нужно:** одна и та же форма схемы раскатывается на арендатора, на
окружение или на тест — один каталог файлов, несколько схем в одной базе.

## Плейсхолдер

`--schema` кладёт журнал в эту схему и одновременно засевает плейсхолдер
`@schema@`, поэтому миграции, написанной под настраиваемую схему, больше ничего
не нужно:

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

Схема создаётся, если её нет. У каждого арендатора свой журнал:

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

## Арендаторы не стоят в очереди друг за другом

Advisory-лок, на котором сериализуются прогоны, выводится из имён схемы и
таблицы, поэтому у каждой схемы свой:

```console
$ migrator locks --schema tenant_a
  Lock     1835496052/1363501398  (derived from tenant_a.schema_migrations)
  Nobody is holding it.

$ migrator locks --schema tenant_b
  Lock     1835496052/998994211  (derived from tenant_b.schema_migrations)
  Nobody is holding it.
```

Два арендатора мигрируют одновременно. Два прогона против **одного** — нет, и в
этом смысл.

Вывод ключа — часть контракта: сменить его в минорной версии значит разрешить
двум версиям инструмента мигрировать одну схему одновременно. Если нужен свой лок
— например, потому что схему делят два разных инструмента, — задайте его через
`WithLockID`.

## Правила подстановки

Подстановка **текстовая и безусловная**. Она не знает SQL, поэтому:

- плейсхолдер может стоять только там, где стоит идентификатор. `@schema@` внутри
  строкового литерала тоже будет подставлен;
- токен вида `@name@`, оставшийся неразрешённым после подстановки, — это ошибка.
  Опечатка `@tabel@` не должна доехать до PostgreSQL синтаксической ошибкой на
  середине DDL-скрипта;
- **чек-сумма считается по файлу до подстановки**, поэтому два деплоя, смотрящие
  на две схемы, не выглядят как чья-то правка выпущенного файла.

Свои плейсхолдеры приходят из `WithPlaceholders`, и ключ пишется с
разделителями:

```go
migrator.WithPlaceholders(map[string]string{"@tablespace@": "fast_ssd"})
```

Голый `tenant` в качестве ключа был бы слепой заменой подстроки по всему файлу —
переписал бы и `tenants`, и `tenant_id`, — а проверка на неразрешённые токены
этого бы не заметила, потому что искать было бы уже нечего. Такой ключ
отвергается при создании Migrator.

## Прогон по многим арендаторам

Флага `--tenants` сегодня нет. Из шелла:

```bash
psql -Atc "SELECT nspname FROM pg_namespace WHERE nspname LIKE 'tenant_%'" |
while read -r schema; do
    migrator up --schema "$schema" || exit 1
done
```

Две вещи стоит решить до того, как писать этот цикл, потому что в них вся
сложность:

- **сколько одновременно.** Пятьсот арендаторов — это пятьсот соединений, если
  ничем не ограничить. Последовательно медленно, зато предсказуемо;
- **что происходит на первом отказе.** Остановиться — значит оставить ранних
  мигрированными, а остальных нет; продолжить — значит нуждаться в знании, **кто
  именно** упал. «12 из 500 упали» бесполезно, если не сказано какие.

Из Go отдельного помощника на арендатора тоже нет. `Migrator` дёшев и неизменяем,
поэтому стройте по одному на схему:

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

Обратите внимание на `FromPool`: каждый `Up` вынимает из пула одно соединение и
держит его весь прогон этого арендатора.

## Дальше

- [Как библиотека](library.ru.md)
- [В CI и в деплое](ci-and-deploy.ru.md)
