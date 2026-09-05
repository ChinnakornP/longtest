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

# actionlint is resolved to the pinned build rather than to whatever happens to
# be on PATH: which workflow bugs this gate catches depends on the version, and
# CI restores ~/go/bin from a cache that outlives a version bump. `?=` so the
# ACTIONLINT_VERSION in .github/workflows/ci.yml (which also keys that cache)
# wins there; this is the value a laptop gets.
ACTIONLINT_VERSION ?= v1.7.12
ACTIONLINT   := $(shell $(GO) env GOPATH)/bin/actionlint

# Workflow files parked outside .github/workflows/ - the agent token has no
# `workflow` scope, so they wait for a human to apply them. GitHub does not
# validate a file at such a path, and neither does a bare `actionlint` run:
# that is exactly how the expression bug in run 33937560568 reached main.
PARKED_WORKFLOWS := $(wildcard .github/*-pending/*.yml .github/*-pending/*.yaml)

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
gen-schema: node_modules ## Generate Go + TS types from packages/qa-schema
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
lint: lint-go lint-js lint-ci ## Lint everything

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

.PHONY: lint-ci
lint-ci: ## actionlint: validate the GitHub Actions workflow files
	@if [ "$$($(ACTIONLINT) -version 2>/dev/null | head -1)" != "$(ACTIONLINT_VERSION)" ]; then \
	  echo "==> installing actionlint $(ACTIONLINT_VERSION)"; \
	  $(GO) install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION); \
	fi
	@# actionlint shells out to shellcheck for every `run:` block when it can
	@# find it. ubuntu-latest always can, a laptop often cannot - and a silent
	@# skip means a local `make lint` is a weaker gate than CI, so say so.
	@command -v shellcheck >/dev/null 2>&1 \
	  || echo "note: shellcheck not installed, skipping the run: block checks that CI does run."
	@echo "==> actionlint .github/workflows"
	@$(ACTIONLINT)
	@# One command per group, never a loop: a `for` here would report the exit
	@# status of its last iteration only (see LONG-22).
	@if [ -n "$(PARKED_WORKFLOWS)" ]; then \
	  echo "==> actionlint $(PARKED_WORKFLOWS)"; \
	  $(ACTIONLINT) $(PARKED_WORKFLOWS); \
	fi

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
	cd daemon && $(GO) run ./cmd/qa-daemon

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
	$(GO) install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

.PHONY: test-security
test-security: ## Run the injection corpus and the security boundary tests
	@cd daemon && $(GO) test ./security/... ./agent/prompts/... -count=1
	@$(PNPM) --filter @qa/executor exec vitest run test/untrusted.test.ts

.PHONY: gen-vectors
gen-vectors: ## Regenerate the Go/TypeScript untrusted-framing parity vectors
	cd daemon && UPDATE_VECTORS=1 $(GO) test ./security/ -run TestUntrustedParityVectors
	@echo "re-run 'make test-security' to check the TypeScript side against them"

.PHONY: injection-corpus
injection-corpus: node_modules ## Serve the hostile fixture app (live injection runs)
	$(PNPM) --filter @qa/injection-corpus run start

.PHONY: scan-secrets
scan-secrets: ## Run the same secret scan CI runs
	docker run --rm -v "$(CURDIR):/repo:ro" -w /repo \
	  ghcr.io/gitleaks/gitleaks:v8.30.0 \
	  dir --no-banner --redact --config /repo/.gitleaks.toml .
