BINARY := frostgate
PKG    := ./cmd/frostgate
VERSION ?= dev

LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build test race vet fmt check run dist clean

build: ## Build the binary for the host platform
	go build $(LDFLAGS) -o $(BINARY) $(PKG)

test: ## Run tests
	go test ./...

race: ## Run tests with the race detector
	go test -race ./...

vet: ## Static analysis
	go vet ./...

fmt: ## Format code
	gofmt -w .

check: fmt vet test race ## fmt, vet, test, race, build
	go build -o /tmp/frostgate-check $(PKG)
	@test -z "$$(gofmt -l . | grep -v vendor)" || (echo "gofmt needed"; gofmt -l .; exit 1)

run: ## Build and run
	go run $(LDFLAGS) $(PKG)

dist: ## Cross-compile static binaries (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64)
	mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(BINARY)-linux-amd64 $(PKG)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(BINARY)-linux-arm64 $(PKG)
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(BINARY)-darwin-amd64 $(PKG)
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(BINARY)-darwin-arm64 $(PKG)
	cd dist && sha256sum $(BINARY)-linux-amd64 $(BINARY)-linux-arm64 $(BINARY)-darwin-amd64 $(BINARY)-darwin-arm64 > SHA256SUMS && cat SHA256SUMS

clean: ## Remove build artifacts
	rm -f $(BINARY) $(BINARY).exe /tmp/frostgate-check
	rm -rf dist
