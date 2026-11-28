.PHONY: build build-tray lint test test-race coverage

LDFLAGS := -ldflags="$(shell . ./packaging/release-env.sh && printf '%s' "$$LDFLAGS")"

build:
	CGO_ENABLED=0 go build $(LDFLAGS) ./cmd/monbooru

build-tray:
	go build -tags tray $(LDFLAGS) ./cmd/monbooru

lint:
	golangci-lint run

test:
	go test -timeout 30m ./...

test-race:
	go test -race -timeout 30m ./...

coverage:
	go test -timeout 30m -coverpkg=./... -coverprofile=coverage.out $(shell go list ./... | grep -v '/cmd/')
	go tool cover -html=coverage.out -o coverage.html
