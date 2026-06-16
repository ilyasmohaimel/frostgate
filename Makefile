BINARY := frostgate
PKG    := ./cmd/frostgate

.PHONY: build test vet run clean dist

build: ## Build the binary for the host platform
	go build -o $(BINARY) $(PKG)

test: ## Run tests with the race detector
	go test -race ./...

vet: ## Static analysis
	go vet ./...

run: ## Build and run against ./frostgate.yaml
	go run $(PKG)

dist: ## Cross-compile static binaries for the homelab and the Tokyo VM
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dist/$(BINARY)-linux-amd64 $(PKG)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o dist/$(BINARY)-linux-arm64 $(PKG)

clean: ## Remove build artifacts
	rm -f $(BINARY) $(BINARY).exe
	rm -rf dist
