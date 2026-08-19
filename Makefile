# Сборка, проверки и локальное окружение db-migrator v2.
#
# .PHONY объявлен и заполнен намеренно. В v1 эта строка была закомментирована,
# и цель, чьё имя совпадало с каталогом, молча переставала работать.

APP           ?= migrator
PKG           ?= github.com/efureev/db-migrator/v2
BUILD_DIR     ?= build

# Версия линтера пиньится: соседний патч анализирует иначе, и «зелёно у меня»
# перестаёт значить «зелёно в CI». В v1 версия бралась из сервиса docker-compose.
#
# Линтер намеренно НЕ добавлен в go.mod через tool-директиву: маленькое дерево
# зависимостей — часть того, что эта библиотека обещает, и `go mod graph`
# у потребителя не должен содержать линтер.
GOLANGCI_VERSION ?= 2.12.2

# Держать в одной паре с docker-compose.yml. Порт нестандартный намеренно:
# локальный PostgreSQL на 5432 не должен оказаться той базой, по которой
# прогоняются деструктивные тесты. См. комментарий в docker-compose.yml о том,
# почему это не 55432.
PGPORT        ?= 55439
MIGRATOR_TEST_DSN ?= postgres://migrator:migrator@127.0.0.1:$(PGPORT)/migrator_test?sslmode=disable

.PHONY: help all fmt vet lint check test test-race test-integration test-all \
        coverage db-up db-down db-logs build build-all docker docker-shell clean tidy

all: help

help: ## Показать этот список
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

fmt: ## Отформатировать исходники
	gofmt -s -w .

vet: ## go vet
	go vet ./...

lint: ## Линтер (версия сверяется с пиненой)
	@have=$$(golangci-lint version --short 2>/dev/null || echo none); \
	if [ "$$have" != "$(GOLANGCI_VERSION)" ]; then \
	  echo "нужен golangci-lint $(GOLANGCI_VERSION), найден: $$have"; \
	  echo "поставить: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_VERSION)"; \
	  exit 1; \
	fi
	golangci-lint run ./...

tidy: ## go mod tidy и проверка, что он ничего не изменил
	go mod tidy
	@git diff --exit-code go.mod go.sum || (echo "go mod tidy изменил файлы — закоммить их" && exit 1)

test: ## Юнит-тесты
	go test ./...

test-race: ## Юнит-тесты под детектором гонок
	go test -race ./...

check: fmt vet lint test-race ## Полный гейт перед коммитом

test-integration: ## Интеграционные тесты (нужна живая PostgreSQL, см. db-up)
	MIGRATOR_TEST_DSN="$(MIGRATOR_TEST_DSN)" go test -tags integration -race ./...

test-all: test-race test-integration ## Все уровни

coverage: ## Покрытие с порогами по пакетам
	MIGRATOR_TEST_DSN="$(MIGRATOR_TEST_DSN)" ./coverage.sh

db-up: ## Поднять PostgreSQL для тестов
	docker compose up -d --wait postgres

db-down: ## Остановить и удалить PostgreSQL
	docker compose down -v

db-logs: ## Логи тестовой PostgreSQL
	docker compose logs -f postgres

build: ## Собрать бинарник под текущую платформу
	./build.sh

build-all: ## Собрать все релизные платформы
	BUILD_ALL=1 ./build.sh

docker: ## Собрать docker-образ (distroless)
	docker build --target distroless -t $(APP):dev .

docker-shell: ## Собрать вариант с шеллом
	docker build --target shell -t $(APP):dev-alpine .

clean: ## Удалить артефакты сборки
	rm -rf $(BUILD_DIR) coverage.out
