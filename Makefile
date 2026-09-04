# AI QA Agent platform - developer entrypoints.
# Every target is expected to work on a clean checkout with only
# docker, go and pnpm installed.

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

COMPOSE      ?= docker compose
GO           ?= go
PNPM         ?= pnpm
S3_BUCKET    ?= qa-artifacts

# golangci-lint is optional locally; CI always installs the pinned version.
GOLANGCI     := $(shell command -v golangci-lint 2>/dev/null || echo "$$(go env GOPATH)/bin/golangci-lint")
GO_MODULES   := server daemon

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# local stack
# ---------------------------------------------------------------------------

.env:
	@cp .env.example .env
	@echo "created .env from .env.example (git-ignored, local dev values only)"

.PHONY: up
up: .env ## Start postgres + minio and create the artifact bucket
	$(COMPOSE) up -d --wait postgres minio
	$(COMPOSE) --profile init run --rm --quiet-pull createbuckets
	@$(COMPOSE) ps

.PHONY: down
down: ## Stop the local stack (volumes are kept)
	$(COMPOSE) --profile init down --remove-orphans

.PHONY: clean
clean: ## Stop the local stack and delete its volumes (destroys local data)
	$(COMPOSE) --profile init down --remove-orphans --volumes

.PHONY: logs
logs: ## Tail the local stack logs
	$(COMPOSE) logs -f --tail=100

# ---------------------------------------------------------------------------
# database
# ---------------------------------------------------------------------------

# Database recipes source .env themselves so `make migrate-up` works straight
# after `make up`, without the developer exporting DATABASE_URL by hand.
DOTENV = set -a; if [ -f .env ]; then . ./.env; fi; set +a;

.PHONY: migrate-up
migrate-up: .env ## Apply database migrations
	@$(DOTENV) cd server && $(GO) run ./cmd/migrate up

.PHONY: migrate-down
migrate-down: .env ## Roll back every migration (leaves an empty database)
	@$(DOTENV) cd server && $(GO) run ./cmd/migrate down-all

.PHONY: migrate-down-one
migrate-down-one: .env ## Roll back only the most recent migration
	@$(DOTENV) cd server && $(GO) run ./cmd/migrate down

.PHONY: migrate-status
migrate-status: .env ## Show which migrations are applied
	@$(DOTENV) cd server && $(GO) run ./cmd/migrate status

.PHONY: seed
seed: .env ## Seed a dev database with one org, one owner and one project
	@$(DOTENV) cd server && $(GO) run ./cmd/seed

# ---------------------------------------------------------------------------
# codegen
# ---------------------------------------------------------------------------

.PHONY: gen
gen: gen-schema gen-sqlc ## Run every code generator

.PHONY: gen-schema
gen-schema: ## Generate Go + TS types from packages/qa-schema
	$(PNPM) --filter @qa/schema run gen

.PHONY: gen-sqlc
gen-sqlc: ## Generate the sqlc database layer from migrations/ + queries/
	@if ! command -v sqlc >/dev/null 2>&1; then \
	  echo "sqlc not installed: run 'make tools'" >&2; exit 1; \
	else \
	  cd server && sqlc generate; \
	fi

# ---------------------------------------------------------------------------
# quality gates
# ---------------------------------------------------------------------------

.PHONY: lint
lint: lint-go lint-js ## Lint everything

.PHONY: lint-go
lint-go: ## gofmt + go vet + golangci-lint on every Go module
	@for m in $(GO_MODULES); do \
	  echo "==> gofmt $$m"; \
	  out=$$(cd $$m && gofmt -l .); \
	  if [ -n "$$out" ]; then echo "not gofmt'd:"; echo "$$out"; exit 1; fi; \
	  echo "==> go vet $$m"; \
	  (cd $$m && $(GO) vet ./...); \
	done
	@if [ -x "$(GOLANGCI)" ]; then \
	  for m in $(GO_MODULES); do \
	    echo "==> golangci-lint $$m"; \
	    (cd $$m && "$(GOLANGCI)" run ./...); \
	  done; \
	else \
	  echo "note: golangci-lint not installed, ran gofmt + go vet only."; \
	  echo "      install it with: make tools"; \
	fi

.PHONY: lint-js
lint-js: node_modules ## eslint + tsc across the pnpm workspace
	$(PNPM) run lint
	$(PNPM) run typecheck

.PHONY: test
test: test-go test-js ## Run every test suite

.PHONY: test-go
test-go: ## go test on every Go module (database-backed tests skip)
	@for m in $(GO_MODULES); do \
	  echo "==> go test $$m"; \
	  (cd $$m && $(GO) test ./...); \
	done

.PHONY: test-db
test-db: .env ## go test with a real Postgres (needs `make up` + `make migrate-up`)
	@$(DOTENV) export TEST_DATABASE_URL="$${TEST_DATABASE_URL:-$$DATABASE_URL}"; \
	  cd server && $(GO) test ./... -count=1

.PHONY: test-js
test-js: node_modules ## vitest across the pnpm workspace
	$(PNPM) run test

.PHONY: fmt
fmt: ## Format Go sources in place
	@for m in $(GO_MODULES); do (cd $$m && gofmt -w .); done

# ---------------------------------------------------------------------------
# dev servers (run from source - the daemon must reach your own network)
# ---------------------------------------------------------------------------

.PHONY: dev-web
dev-web: node_modules ## Run the Next.js dev server
	$(PNPM) --filter @qa/web run dev

.PHONY: dev-server
dev-server: ## Run the Go backend
	cd server && $(GO) run ./cmd/server

.PHONY: dev-daemon
dev-daemon: ## Run the QA daemon
	cd daemon && $(GO) run ./cmd/daemon

# ---------------------------------------------------------------------------
# setup
# ---------------------------------------------------------------------------

node_modules: package.json pnpm-lock.yaml
	$(PNPM) install --frozen-lockfile
	@touch node_modules

.PHONY: install
install: ## Install JS dependencies
	$(PNPM) install

.PHONY: tools
tools: ## Install the pinned Go developer tooling
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2
	$(GO) install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0

.PHONY: scan-secrets
scan-secrets: ## Run the same secret scan CI runs
	docker run --rm -v "$(CURDIR):/repo:ro" -w /repo \
	  ghcr.io/gitleaks/gitleaks:v8.30.0 \
	  dir --no-banner --redact --config /repo/.gitleaks.toml .
