SHELL := /bin/bash

TAG ?= $(shell git describe --tags --exact-match 2>/dev/null)
COMMIT_HASH := $(shell git rev-parse --short HEAD 2>/dev/null)

# Append "-dirty" to the version if there are uncommitted changes
DIRTY = $(shell test -z "$$(git status --porcelain 2>/dev/null)" && echo "" || echo "-dirty")

# Derive version: if TAG is not empty use it; else use dev-<COMMIT_HASH>; final fallback 'dev'.
VERSION = $(if $(TAG),$(TAG)$(DIRTY),$(if $(COMMIT_HASH),dev-$(COMMIT_HASH)$(DIRTY),dev))
MIN_COVER ?= 40
OS_ARCHES := linux/amd64
BINARY := mbkp
BIN_DIR := bin
DIST_DIR := dist
PKG := ./cmd/mbkp

# Default target
.PHONY: help
help: ## Show this help
	@echo "tablo Makefile targets"; echo; awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_.-]+:.*##/ { printf "  %-20s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the CLI binary (local OS/ARCH)
	@echo "Building $(BINARY) (version $(VERSION))"
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN_DIR)/$(BINARY) $(PKG)
	@echo "Built: $(BIN_DIR)/$(BINARY)"

.PHONY: install
install: ## Install the binary into GOPATH/bin (or GOBIN)
	@echo "Installing $(BINARY) (version $(VERSION))"
	CGO_ENABLED=0 go install -trimpath -ldflags="-s -w -X main.version=$(VERSION)" $(PKG)

.PHONY: run
run: build ## Run the CLI with --help (example)
	./$(BIN_DIR)/$(BINARY) --help

.PHONY: tidy
tidy: ## Run go mod tidy (and verify no drift)
	@go mod tidy
	@git diff --quiet -- go.mod go.sum || (echo "go.mod/go.sum changed; commit the updates" && exit 1)

.PHONY: lint
lint: ## Run linters (golangci-lint if available, else go vet/gofmt/staticcheck)
	@if command -v golangci-lint >/dev/null 2>&1; then \
	  echo "Running golangci-lint"; \
	  golangci-lint run ./...; \
	else \
	  echo "golangci-lint not found; running go vet"; \
	  go vet ./...; \
	fi
	@echo "Checking formatting (gofmt)"; \
	fmt_out=$$(gofmt -l . | grep -v '^vendor/' || true); \
	if [ -n "$$fmt_out" ]; then echo "Files need formatting:"; echo "$$fmt_out"; exit 1; fi
	@if command -v staticcheck >/dev/null 2>&1; then \
	  echo "Running staticcheck"; staticcheck ./...; \
	else \
	  echo "staticcheck not found (optional)"; \
	fi

.PHONY: test
test: ## Run tests
	@go test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests with coverage profile (text summary)
	@go test -race -covermode=atomic -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tee coverage.func.txt
	@total=$$(go tool cover -func=coverage.out | grep '^total:' | awk '{print $$3}' | sed 's/%//'); \
	  total=$${total:-0}; \
	  awk 'BEGIN { if ('"$$total"' < $(MIN_COVER)) { printf "Coverage %.2f%% is below MIN_COVER=%.2f%%\n", '"$$total"', $(MIN_COVER); exit 1 } else { printf "Coverage %.2f%% (min %.2f%%) OK\n", '"$$total"', $(MIN_COVER) } }'

.PHONY: cover-html
cover-html: cover ## Generate HTML coverage report
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Open coverage.html in your browser."

.PHONY: clean
clean: ## Clean up build artifacts
	@rm -rf $(BIN_DIR) $(DIST_DIR) coverage.out coverage.func.txt coverage.html
	@go clean

.PHONY: release-check
release-check: ## Validate git state before release (not DIRTY, TAG not empty, TAG exists)
	@test -z "$(DIRTY)" || (echo "Working tree is dirty"; exit 1)
	@if [ -z "$(TAG)" ]; then echo "No git tag found for current commit; cannot release"; exit 1; fi
	@if ! git rev-parse "$(TAG)" >/dev/null 2>&1; then echo "Tag $(TAG) does not exist"; exit 1; fi

.PHONY: release
release: release-check ## Build release archives for multiple OS/ARCH into dist/
	@echo "Building release artifacts for version $(VERSION)"
	@mkdir -p $(DIST_DIR)
	@rm -f $(DIST_DIR)/sha256sums.txt
	@set -e; \
	  for target in $(OS_ARCHES); do \
	    os=$${target%/*}; arch=$${target##*/}; \
	    ext=""; if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
	    out="$(DIST_DIR)/$(BINARY)-$(VERSION)-$$os-$$arch$$ext"; \
	    echo "  -> $$out"; \
	    GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o "$$out" $(PKG); \
	    sha256sum "$$out" >> $(DIST_DIR)/sha256sums.txt; \
	  done; \
	  sort -o $(DIST_DIR)/sha256sums.txt $(DIST_DIR)/sha256sums.txt
	@echo "Artifacts in $(DIST_DIR)/"
	@echo "SHA256 sums:"; cat $(DIST_DIR)/sha256sums.txt

.PHONY: tag
tag: ## Create and push git tag $(TAG)
	@test -z "$(DIRTY)" || (echo "Working tree is dirty"; exit 1)
	@if [ -z "$(TAG)" ]; then echo "TAG is empty"; exit 1; fi
	@if git rev-parse "$(TAG)" >/dev/null 2>&1; then echo "Tag $(TAG) already exists"; exit 1; fi
	git tag -a "$(TAG)" -m "Release $(VERSION)"
	git push origin "$(TAG)"

.PHONY: print-version
print-version: ## Print detected version
	@echo $(VERSION)

.PHONY: ci
ci: tidy lint test cover ## Run typical CI pipeline
	@echo "CI pipeline complete."
