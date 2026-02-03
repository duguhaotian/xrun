# MicroVM Sandbox Makefile
# See AGENTS.md for detailed project guidelines

# Variables
GO := go
GOPATH := $(shell $(GO) env GOPATH)
LDFLAGS := -ldflags="-s -w"
BIN_DIR := bin
CMD_DIR := cmd

# Auto-detect all binaries from cmd/ directory
CMD_PACKAGES := $(wildcard $(CMD_DIR)/*)
BINARIES := $(notdir $(CMD_PACKAGES))
BINARY_PATHS := $(addprefix $(BIN_DIR)/,$(BINARIES))

# Default target
.PHONY: all
all: build

# Build all binaries
.PHONY: build
build: $(BINARY_PATHS) ## Build all binaries from cmd/*

# Pattern rule to build each binary
$(BIN_DIR)/%: $(CMD_DIR)/%/main.go $(CMD_DIR)/%/*.go
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $@ ./$(CMD_DIR)/$*

# Build production binaries (stripped)
.PHONY: build-prod
build-prod: ## Build production binaries (stripped)
	@mkdir -p $(BIN_DIR)
	@for binary in $(BINARIES); do \
		echo "Building $$binary (production)..."; \
		$(GO) build $(LDFLAGS) -o $(BIN_DIR)/$$binary ./$(CMD_DIR)/$$binary; \
	done

# Generate build and run targets for each binary
define BUILD_RULE
.PHONY: build-$(1)
build-$(1): $(BIN_DIR)/$(1) ## Build $(1) binary
endef

define RUN_RULE
.PHONY: run-$(1)
run-$(1): $(BIN_DIR)/$(1) ## Build and run $(1) binary
	./$(BIN_DIR)/$(1)
endef

$(foreach binary,$(BINARIES),$(eval $(call BUILD_RULE,$(binary))))
$(foreach binary,$(BINARIES),$(eval $(call RUN_RULE,$(binary))))

# Build all packages
.PHONY: build-all
build-all: ## Build all packages
	$(GO) build ./...

# Test targets
.PHONY: test
test: ## Run all tests
	$(GO) test ./...

.PHONY: test-race
test-race: ## Run tests with race detector
	$(GO) test -race ./...

.PHONY: test-cover
test-cover: ## Run tests with coverage
	$(GO) test -cover ./...

.PHONY: test-cover-html
test-cover-html: ## Generate HTML coverage report
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out

.PHONY: test-verbose
test-verbose: ## Run tests with verbose output
	$(GO) test -v ./...

# Lint and format targets
.PHONY: fmt
fmt: ## Format code with go fmt
	$(GO) fmt ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: fmt vet ## Run all linting (fmt + vet)
	@echo "Running go fmt and go vet..."

.PHONY: lint-full
lint-full: lint ## Run full linting (requires golangci-lint)
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed, skipping..."; \
	fi

.PHONY: lint-fix
lint-fix: ## Fix auto-fixable issues (requires golangci-lint)
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run --fix; \
	else \
		echo "golangci-lint not installed, run: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
	fi

.PHONY: imports
imports: ## Check and fix imports (requires goimports)
	@if command -v goimports >/dev/null 2>&1; then \
		goimports -w .; \
	else \
		echo "goimports not installed, run: go install golang.org/x/tools/cmd/goimports@latest"; \
	fi

# Development targets
.PHONY: clean
clean: ## Clean build artifacts
	@rm -rf $(BIN_DIR)/
	@rm -f coverage.out

.PHONY: deps
deps: ## Download dependencies
	$(GO) mod download
	$(GO) mod tidy

.PHONY: verify
verify: ## Verify dependencies
	$(GO) mod verify

# Quality assurance
.PHONY: check
check: lint test ## Run all checks (lint + test)
	@echo "All checks passed!"

.PHONY: ci
ci: lint-full test-race build-all ## Run CI pipeline (lint + race tests + build)
	@echo "CI checks passed!"

# Help target
.PHONY: help
help: ## Show this help message
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Detected binaries: $(BINARIES)"
	@echo ""
	@echo "Dynamic targets:"
	@echo "  $(BIN_DIR)/<name>    Build specific binary"
	@echo "  build-<name>         Build specific binary (alias)"
	@echo "  run-<name>           Build and run specific binary"
