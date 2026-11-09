.PHONY: build build-tagger build-tray lint test test-race test-tagger coverage coverage-tagger

LDFLAGS := -ldflags="$(shell . ./packaging/release-env.sh && printf '%s' "$$LDFLAGS")"

build:
	go build $(LDFLAGS) ./cmd/monbooru

build-tagger:
	go build -tags tagger $(LDFLAGS) ./cmd/monbooru

build-tray:
	go build -tags tray $(LDFLAGS) ./cmd/monbooru

lint:
	golangci-lint run

test:
	go test -timeout 30m ./...

test-race:
	go test -race -timeout 30m ./...

TAGGER_ENV = $(if $(MODELS),MONBOORU_PATHS_MODEL_PATH=$(MODELS)) $(if $(MODELS_EVAL),MONBOORU_MODELS_EVAL=$(MODELS_EVAL))

test-tagger:
	$(TAGGER_ENV) go test -tags tagger -timeout 30m ./...

coverage:
	go test -timeout 30m -coverpkg=./... -coverprofile=coverage.out $(shell go list ./... | grep -v '/cmd/')
	go tool cover -html=coverage.out -o coverage.html

coverage-tagger:
	$(TAGGER_ENV) go test -tags tagger -timeout 30m -coverpkg=./... -coverprofile=coverage-tagger.out $(shell go list ./... | grep -v '/cmd/')
	go tool cover -html=coverage-tagger.out -o coverage-tagger.html
