# Защиты в продакшене

*[In English](production-guards.md)*

**Когда нужно:** вы решаете, что инструменту позволено делать с базой, которой
пользуются живые люди.

Защиты — это и есть смысл инструмента, а не его функция. Каждая из них — место,
где очевидное удобство отвергнуто намеренно.

## Откат надо попросить

```console
$ migrator down
migrator: down migrations require WithAllowDown

Rolling back changes the schema of a database something is already running against.
Pass the flag once you have decided that is what you want:

  migrator down --steps 1 --allow-down
exit 6
```

## В продакшене его нет вовсе

```console
$ migrator down --allow-down --env production
migrator: refused in a production environment: rolling back is not available when the environment is production
exit 6
```

Флага, который это отменяет, нет. Если схема в продакшене должна поехать назад —
это миграция вперёд, написанная для этой цели и отревьюенная как любая другая, а
не откат, набранный в спешке.

Окружение берётся из `--env` или `MIGRATOR_ENV`, а когда не задано ни то ни
другое — **выводится** из DSN: не-loopback и не-приватный хост либо
production-подобное имя базы читаются как production. Уклон намеренный. В
деплое ставьте явно и перестаньте полагаться на догадку:

```bash
export MIGRATOR_ENV=production
```

## У стирания три отдельных ключа

`wipe` удаляет все таблицы, вью, последовательности, процедуры и типы в схеме, в
одной транзакции, оставляя саму схему и её расширения в покое.

Ему нужны `--allow-wipe` **и** `--confirm <база>`, и подтверждение сверяется с той
базой, к которой вы действительно подключены:

```console
$ migrator wipe --allow-wipe --confirm uc_guardz
migrator: confirmation does not name this database: confirmation names "uc_guardz", connected to "uc_guards"
exit 6
```

Опечатка — это и есть смысл. `--yes` значит «перестань спрашивать»;
`--confirm <база>` значит «я знаю, что это за база», и это разные утверждения.
Данное подтверждение проверяется **в любом** окружении; от окружения зависит
только то, обязательно ли оно.

Сначала посмотреть:

```console
$ migrator wipe --allow-wipe --confirm uc_guards --dry-run
dry run: nothing will be dropped
  would drop table public.schema_migrations
  would drop table public.users

  2 objects would be dropped, 0 kept in uc_guards
```

Dry-run проходит **те же** ворота, а не меньшие: «что это снесёт» не должно стать
способом спросить без флагов, которые делают ответ осмысленным.

А в продакшене — снова никак:

```console
$ migrator wipe --allow-wipe --confirm uc_guards --env production
migrator: refused in a production environment: wipe is not available when the environment is production
exit 6
```

По умолчанию база, имя которой попадает под `(?i)prod`, защищена и вне
production-окружения. Шаблон — опция библиотеки (`WithWipeProtectPattern`), флага
у неё нет: это свойство того, как инструмент встроен, а не одного запуска.

### Объекты за пределами схемы

`DROP ... CASCADE` на таблице, от которой зависит вью из **другой** схемы, уносит
эту вью — молча. `wipe` сначала обходит `pg_depend` и отказывается, перечисляя,
что ушло бы:

```
  refused: dropping public.users would take reporting.user_summary with it
```

Согласиться на это — отдельное решение и отдельная опция библиотеки:
`WithForceWipe`, а не та, что разрешила стирание. Разрешить снести свою схему и
согласиться потерять чужой объект — разные вещи, и один переключатель на оба
собирал бы второе согласие вместе с первым.

**В CLI флага для этого нет вовсе.** Из командной строки путь мимо межсхемной
зависимости — разобраться с зависимым объектом самому, в той схеме, которой он
принадлежит, где кто-то знает, зачем он нужен.

## Почему у части настроек нет переменной окружения

У `--allow-down`, `--allow-wipe` и `--confirm` её нет намеренно. Переменная
наследуется каждым процессом в контейнере и каждым шагом CI-джобы, а флаг
набирается для одного запуска.

У `--max-lock-level` она есть (`MIGRATOR_MAX_LOCK_LEVEL`) по противоположной
причине: это политика, которую ставят один раз на pipeline и оставляют. Она
ограничивает, а не разрешает.

## Права, проверенные до первого оператора

Прежде чем что-либо выполнится, инструмент проверяет, что роль может создавать в
той схеме, на которую его направили:

```
migrator: insufficient privilege: role "app_ro" may not create in schema "public"
exit 6
```

Узнать о нехватке права на третьей миграции — худший момент, особенно если первые
две уже применились. `status` и `version` эту проверку не делают: они ничего не
создают и остаются доступны роли только на чтение.

## Что не защищено и защищено быть не может

`adopt` записывает в журнал утверждение, которое потом никто не перепроверит, и
верит вам на слово. Это его единственный риск, и `--confirm` вне development —
единственное, что там стоит. См.
[Приём существующей базы](adopt-existing-database.ru.md).

## Дальше

- [В CI и в деплое](ci-and-deploy.ru.md)
- [Безопасное изменение схемы](safe-schema-change.ru.md) — защита, которая читает
  SQL, а не флаги
