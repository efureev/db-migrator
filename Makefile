PROJECT_NAME := "migrator"
PKG := "$(PROJECT_NAME)"

#REGISTRY_HOST := "git.sitesoft.ru:4567"
#CI_PROJECT_PATH := ssp/services/captcha
#CONTAINER_IMAGE := "git.sitesoft.ru/${CI_PROJECT_PATH}"
CONTAINER_IMAGE := "${PROJECT_NAME}"

VERSION_BUILD=$(shell git log --pretty="%h" -n1 HEAD)
VERSION_TAG=$(shell git describe --abbrev=0 --tags)
BUILD_TIME?=$(shell date -u '+%Y-%m-%dT%H:%M:%S')

PKG_LIST := $(shell go list ${PKG}/... | grep -v /vendor/)
GO_FILES := $(shell find . -name '*.go' | grep -v /vendor/ | grep -v _test.go)
BUILD_PATH :="build"

#VERSION_COMMIT :=  $(git log --pretty="%h" -n1 HEAD)
#VERSION_DEFAULT := $(git tag --sort=-v:refname --list "v[0-9]*" | head -n 1)

.PHONY: all dep d-build build test coverage coverhtml lint

all: build

lint: ## Lint the files
	@golint -set_exit_status ${PKG_LIST}

test: ## Run unittests
	@go test -short ${PKG_LIST}

race: dep ## Run data race detector
	@go test -race -short ${PKG_LIST}

msan: dep ## Run memory sanitizer
	@go test -msan -short ${PKG_LIST}

coverage: ## Generate global code coverage report
	./.tools/coverage.sh;

coverhtml: ## Generate global code coverage report in HTML
	./.tools/coverage.sh html;

dep: ## Get the dependencies
	@go get -v -d ./...

build: dep ## Build the binary file
#	CGO_ENABLED=0 GOOS=linux GOARCH=x64 go build -a -installsuffix cgo -ldflags="-X captcha/src/config.version=$(VERSION_BUILD)" -o $(BUILD_PATH)/$(PROJECT_NAME) ;
	./build.sh

docker: ## Build the binary file
#	docker build --build-arg SSH_PRIVATE_KEY --build-arg SOCIAL_SECRET --tag $CONTAINER_IMAGE:$NOW_TIMESTAMP --tag $CONTAINER_IMAGE:latest --cache-from $CONTAINER_IMAGE:latest .
	docker build \
		--build-arg VERSION_DEFAULT="${VERSION_TAG}" \
		--build-arg BUILD_TIME="${BUILD_TIME}" \
		--tag migrator:latest -f ./.ci/Dockerfile .

help: ## Display this help screen
	@grep -h -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'
