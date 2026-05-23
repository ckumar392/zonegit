.PHONY: all build test test-race lint vet tidy clean demo bench cover coredns help

GO         ?= go
PKG        := ./...
BINDIR     := bin
LDFLAGS    := -s -w
COVERAGE   := coverage.out

all: lint test build ## Lint, test, and build

build: ## Build all binaries into ./bin
	@mkdir -p $(BINDIR)
	$(GO) build -ldflags="$(LDFLAGS)" -o $(BINDIR)/zonegit  ./cmd/zonegit
	$(GO) build -ldflags="$(LDFLAGS)" -o $(BINDIR)/zonegitd ./cmd/zonegitd

test: ## Run unit tests
	$(GO) test -count=1 $(PKG)

test-race: ## Run tests with the race detector
	$(GO) test -race -count=1 $(PKG)

bench: ## Run benchmarks
	$(GO) test -run=^$$ -bench=. -benchmem -count=5 $(PKG)

cover: ## Run tests with coverage report
	$(GO) test -coverprofile=$(COVERAGE) $(PKG)
	$(GO) tool cover -func=$(COVERAGE) | tail -n 1

vet: ## go vet
	$(GO) vet $(PKG)

lint: vet ## Run linters (golangci-lint if installed, else go vet)
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo ">> golangci-lint not installed, ran go vet only"; \
	fi

tidy: ## go mod tidy
	$(GO) mod tidy

demo: build ## End-to-end demo: import, commit, dig, edit, dig
	./scripts/demo.sh

coredns: ## Build a custom CoreDNS binary with the zonegit plugin (separate Go module; first build pulls in CoreDNS deps)
	@mkdir -p $(BINDIR)
	cd cmd/coredns-with-zonegit && $(GO) build -o ../../$(BINDIR)/coredns
	@echo ">> built $(BINDIR)/coredns with zonegit plugin"

clean: ## Remove build artifacts
	rm -rf $(BINDIR) $(COVERAGE) /tmp/zonegit-demo

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
