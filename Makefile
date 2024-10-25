APP ?= "migrator"
PKG ?= "github.com/efureev/db-migrator"

TEST_FLAGS ?=
BUILD_PATH ?= build
COVERAGE_DIR ?= .coverage

VERSION_BUILD=$(shell git log --pretty="%h" -n1 HEAD)
VERSION_TAG=$(shell git describe --abbrev=0 --tags)
BUILD_TIME?=$(shell date -u '+%Y-%m-%dT%H:%M:%S')

PKG_LIST := $(shell go list ${PKG}/... | grep -v /vendor/)
GO_FILES := $(shell find . -name '*.go' | grep -v /vendor/ | grep -v _test.go)

#VERSION_COMMIT :=  $(git log --pretty="%h" -n1 HEAD)
#VERSION_DEFAULT := $(git tag --sort=-v:refname --list "v[0-9]*" | head -n 1)

#.PHONY: all dep d-build build test coverage coverhtml lint

all: help

lint: ## Lint the files
	@golint -set_exit_status ${PKG_LIST}

test-short:
	make test-with-flags --ignore-errors TEST_FLAGS='-short'

test:
	@-rm -r $(COVERAGE_DIR)
	@mkdir $(COVERAGE_DIR)
	make test-with-flags TEST_FLAGS='-v -race -covermode atomic -coverprofile $(COVERAGE_DIR)/combined.txt -bench=. -benchmem -timeout 20m'

test-with-flags:
	@go test $(TEST_FLAGS) ./...

race: dep ## Run data race detector
	@go test -race -short ${PKG_LIST}

msan: dep ## Run memory sanitizer
	@go test -msan -short ${PKG_LIST}

html-coverage:
	go tool cover -html=$(COVERAGE_DIR)/combined.txt

clean:
	-rm -r "./${BUILD_PATH}"

build: ## Build the binary file
	APP_NAME=migrator ./build.sh

build-docker:
	BUILD_FOR_DOCKER=1 ./build.sh

build-docker-image:
	docker build \
		--build-arg TARGET="local" \
		--build-arg VERSION_TAG="${VERSION_TAG}" \
		--build-arg APP_NAME="migrate" \
		--tag efureev/db-migrator:latest \
		--progress plain \
		-f ./.ci/Dockerfile \
		.

# example: make release V=0.0.0
release:
	git tag v$(V)
	@read -p "Press enter to confirm and push to origin ..." && git push origin v$(V)


#run: container
#    (docker stop "${APP}:${VERSION_TAG}" || true) && (docker rm "${APP}:${VERSION_TAG}" || true)
#    docker exec --name "${APP}" --rm \
#    	"${APP}:${VERSION_TAG}"

help: ## Display this help screen
	@grep -h -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

