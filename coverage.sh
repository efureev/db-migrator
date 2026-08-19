#!/usr/bin/env bash
#
# Проверка покрытия по пакетам против .coverage-thresholds.
#
# Общий порог на репозиторий скрывает именно то, что важно: падение покрытия
# домена компенсируется ростом в другом месте, и цифра остаётся зелёной.
# Поэтому планка у каждого пакета своя.

set -euo pipefail

THRESHOLDS="${THRESHOLDS:-.coverage-thresholds}"
PROFILE="${PROFILE:-coverage.out}"
MODULE="github.com/efureev/db-migrator/v2"

if [[ ! -f "$THRESHOLDS" ]]; then
  echo "не найден файл порогов: $THRESHOLDS" >&2
  exit 2
fi

TAGS=()
if [[ -n "${MIGRATOR_TEST_DSN:-}" ]]; then
  # Без живой БД internal/store покрыт быть не может, и считать его «как
  # получилось» — худший исход: планка опустилась бы до значения, которое
  # ничего не гарантирует. Поэтому либо считаем с интеграционным уровнем,
  # либо честно сообщаем, что часть порогов не проверена.
  TAGS=(-tags integration)
else
  echo "MIGRATOR_TEST_DSN не задан: пороги пакетов, покрываемых только"
  echo "интеграционным уровнем, проверены не будут. make db-up поднимет базу."
  echo
fi

# -covermode=atomic, а не set: тесты конкурентные, и под -race счётчики без
# атомарности дают неверные числа.
#
# -coverpkg=./... обязателен: e2e и интеграционные тесты живут в других
# пакетах, чем код, который они исполняют.
#
# Покрытие собирается в бинарном формате в один каталог из двух источников:
# самих тестов (-test.gocoverdir) и ИНСТРУМЕНТИРОВАННОГО БИНАРНИКА, который
# пакет e2e запускает подпроцессом.
#
# Без второго источника команды CLI, которым нужна БД, выглядят непокрытыми:
# Go не собирает покрытие чужого процесса, если тот не собран с -cover и не
# знает, куда писать. Раньше планка internal/cli несла в себе извинение за это.
#
# MIGRATOR_E2E_COVERDIR, а не GOCOVERDIR: `go test` не кладёт GOCOVERDIR в
# окружение тестового процесса, поэтому e2e/TestMain не смог бы его прочитать.
COVERDIR=$(mktemp -d)
trap 'rm -rf "$COVERDIR"' EXIT

MIGRATOR_E2E_COVERDIR="$COVERDIR" go test "${TAGS[@]+"${TAGS[@]}"}" ./... \
  -cover -covermode=atomic -coverpkg=./... -args -test.gocoverdir="$COVERDIR" >/dev/null

# Профиль в текстовом формате собирается из бинарных счётчиков обоих источников:
# самих тестов и подпроцессов, которые они запускали.
go tool covdata textfmt -i="$COVERDIR" -o="$PROFILE" 2>/dev/null || {
  echo "не удалось собрать покрытие из $COVERDIR" >&2
  exit 1
}

# Покрытие считается ПО ОПЕРАТОРАМ из самого профиля, а не усреднением
# процентов по функциям: маленькая функция весит там столько же, сколько
# большая, и число расходится с тем, что показывает `go test -cover`.
#
# Строка профиля: путь/файл.go:начало,конец <число операторов> <счётчик>
#
# Каталог сравнивается ТОЧНО, а не по префиксу: иначе порог internal/store
# считался бы заодно по internal/storetest, а порог корня — по всему модулю.
#
# Блоки ДЕДУПЛИЦИРУЮТСЯ по своему ключу. С -coverpkg=./... каждый тестовый
# бинарник печатает в профиль все блоки модуля, поэтому один и тот же блок
# встречается столько раз, сколько в модуле пакетов с тестами. Наивная сумма
# считает его операторы многократно, а покрытым — только там, где он
# исполнялся, и занижает результат втрое. Берём максимальный счётчик по
# блоку, ровно как это делает `go tool cover`.
coverage_of() {
  local pkg="$1"
  awk -v module="$MODULE/" -v want="$pkg" '
      NR == 1 { next }                       # строка "mode: atomic"
      {
        path = $1
        sub(/:[0-9].*$/, "", path)           # отрезать позицию, остаётся путь файла
        if (index(path, module) != 1) next
        rel = substr(path, length(module) + 1)

        slash = 0
        for (i = length(rel); i > 0; i--) {
          if (substr(rel, i, 1) == "/") { slash = i; break }
        }
        dir = (slash == 0) ? "." : substr(rel, 1, slash - 1)

        if (dir != want) next

        stmts[$1] = $2
        if ($3 > 0 && $3 > hits[$1]) hits[$1] = $3
      }
      END {
        for (k in stmts) {
          total += stmts[k]
          if (hits[k] > 0) covered += stmts[k]
        }
        if (total > 0) printf "%.1f", 100 * covered / total; else print "n/a"
      }
    ' "$PROFILE"
}

failed=0

while read -r pkg want; do
  [[ -z "$pkg" || "$pkg" == \#* ]] && continue

  got=$(coverage_of "$pkg")

  if [[ "$got" == "n/a" ]]; then
    printf '  %-22s НЕТ ДАННЫХ (пакета нет в профиле)\n' "$pkg"
    failed=1
    continue
  fi

  if awk -v g="$got" -v w="$want" 'BEGIN { exit !(g + 0 < w + 0) }'; then
    printf '  %-22s %5s%%  < %s%%  ПОРОГ\n' "$pkg" "$got" "$want"
    failed=1
  else
    printf '  %-22s %5s%%  ≥ %s%%\n' "$pkg" "$got" "$want"
  fi
done < "$THRESHOLDS"

if [[ "$failed" -ne 0 ]]; then
  echo >&2
  echo "покрытие ниже порога. Либо верните тесты, либо правьте .coverage-thresholds" >&2
  echo "тем же коммитом — с объяснением, почему планка опущена." >&2
  exit 1
fi
