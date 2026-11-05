# Common development tasks. Run `make help` for the list.
GO      ?= go
BIN     := bin/pathgauge
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/agoodley/pathgauge/internal/version.Version=$(VERSION)
LAB_IMAGE := pathgauge-lab:local

.DEFAULT_GOAL := help
.PHONY: help build ui test fmt lint licences snapshot install-test e2e lab-smoke lab-scenarios dev-stack clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build bin/pathgauge (static; embeds the dashboard from internal/server/ui/dist)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/pathgauge

ui: ## Build the dashboard (web/) into internal/server/ui/dist
	cd web && npm ci && npm run build

test: ## Run unit tests
	$(GO) test ./...

fmt: ## Format Go code
	gofmt -w .

lint: ## Run golangci-lint (falls back to its Docker image)
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run; \
	else docker run --rm -v "$(CURDIR):/app" -w /app golangci/golangci-lint:latest golangci-lint run; fi

licences: ## Check dependency licences are Apache-2.0 compatible (needs go-licenses)
	go-licenses check ./... --allowed_licenses=Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC

snapshot: ## Build .deb/.rpm/.tar.gz for amd64+arm64 into dist/ (needs goreleaser)
	goreleaser release --snapshot --clean

install-test: ## Install/remove packages on Rocky 8/9/10 and Ubuntu 22.04/24.04/26.04 (Docker)
	./packaging/test/run-install-tests.sh

e2e: ## Packaged server (Rocky 9) + agent (Rocky 8) under systemd, tests via the API (Docker)
	./packaging/test/e2e-server-agent.sh

lab-smoke: ## Run the netns lab smoke test in a privileged container (Docker)
	docker build -q -t $(LAB_IMAGE) lab >/dev/null
	docker run --rm --privileged -v "$(CURDIR)/lab:/lab:ro" $(LAB_IMAGE) /lab/smoke.sh

lab-scenarios: build ## Accuracy scenarios: known faults in the netns lab vs PathGauge's diagnosis (Docker, ~12 min)
	docker build -q -t $(LAB_IMAGE) lab >/dev/null
	docker run --rm --privileged -v "$(CURDIR)/lab:/lab:ro" -v "$(CURDIR)/bin:/pg:ro" $(LAB_IMAGE) /lab/scenarios.sh /pg/pathgauge

dev-stack: ## Start a local server + agent with demo data (https://127.0.0.1:17443, admin / admin-password-123)
	./scripts/dev-stack.sh start

clean: ## Remove build output
	rm -rf bin dist completions
