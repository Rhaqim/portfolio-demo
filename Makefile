# Demo API: common commands. `make` (or `make help`) lists them.
#
# Local:   make up          then, in the portfolio, `make site`
# Checks:  make check       (formatting, vet, tests, configs)
# Server:  make server-up TAG=<sha>   (run from /opt/demo on the Hetzner box)

SHELL := /bin/bash
.DEFAULT_GOAL := help

COMPOSE       := docker compose
LOCAL         := $(COMPOSE) -f docker-compose.yml -f docker-compose.local.yml
GO_DIR        := services/go
USSD_DIR      := services/ussd
CADDY_IMAGE   := caddy:2.10-alpine
# Where the portfolio lives, for `make site` (one level up while demo/ sits inside it).
PORTFOLIO_DIR ?= ..
# Base URL for `make feedback` and `make health`.
API           ?= https://api.rhaqim.com
# Native runs keep their database and media here.
DATA_DIR      ?= ./data

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)} /^[a-zA-Z0-9_-]+:.*?## / {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

##@ Local stack (Docker)

.PHONY: up
up: ## Build and start the whole API on http://localhost:8080
	$(LOCAL) up --build -d
	@echo "API: http://localhost:8080  (run 'make site' for the pages, 'make logs' to follow)"

.PHONY: down
down: ## Stop the local stack (data volume is kept)
	$(LOCAL) down

.PHONY: restart
restart: ## Rebuild and restart one service: make restart s=loom
	@test -n "$(s)" || { echo "usage: make restart s=<gateway|hub|buckt|loom|ussd>"; exit 1; }
	$(LOCAL) up --build -d $(s)

.PHONY: logs
logs: ## Follow logs (all, or one: make logs s=loom)
	$(LOCAL) logs -f --tail 100 $(s)

.PHONY: ps
ps: ## Show local containers
	$(LOCAL) ps

.PHONY: reset
reset: ## Stop the local stack AND delete its data (games, sandboxes, feedback)
	@read -p "Delete the local demo data volume? [y/N] " ok && [ "$$ok" = y ]
	$(LOCAL) down -v

.PHONY: site
site: ## Run the portfolio dev server against the local API
	cd $(PORTFOLIO_DIR) && PUBLIC_DEMO_API=http://localhost:8080 npm run dev

##@ Native runs (no Docker; one terminal each, API ports 8081-8084)

$(DATA_DIR):
	@mkdir -p $(DATA_DIR)

.PHONY: run-hub
run-hub: $(DATA_DIR) ## Run the feedback service on :8084
	cd $(GO_DIR) && DEMO_DB=$(abspath $(DATA_DIR))/demo.db ADDR=127.0.0.1:8084 ADMIN_TOKEN=dev go run ./cmd/hub

.PHONY: run-buckt
run-buckt: $(DATA_DIR) ## Run the buckt sandbox service on :8081
	cd $(GO_DIR) && DEMO_DB=$(abspath $(DATA_DIR))/demo.db BUCKT_MEDIA_DIR=$(abspath $(DATA_DIR))/buckt-media ADDR=127.0.0.1:8081 go run ./cmd/buckt

.PHONY: run-loom
run-loom: $(DATA_DIR) ## Run the loom service on :8082
	cd $(GO_DIR) && DEMO_DB=$(abspath $(DATA_DIR))/demo.db ADDR=127.0.0.1:8082 go run ./cmd/loom

.PHONY: run-ussd
run-ussd: ## Run the USSD service on :8083
	cd $(USSD_DIR) && ADDR=127.0.0.1:8083 cargo run

##@ Checks

.PHONY: check
check: fmt-check vet test caddy-check compose-check ## Everything CI would want: formatting, vet, tests, configs

.PHONY: fmt
fmt: ## Format Go, Rust and the Caddyfiles
	cd $(GO_DIR) && gofmt -w .
	cd $(USSD_DIR) && cargo fmt
	@if command -v docker >/dev/null && docker info >/dev/null 2>&1; then \
		for f in Caddyfile Caddyfile.local; do \
			docker run --rm -v $(abspath gateway):/etc/caddy $(CADDY_IMAGE) caddy fmt --overwrite /etc/caddy/$$f; \
		done; \
	else echo "skipping Caddyfile formatting (Docker isn't running)"; fi

.PHONY: fmt-check
fmt-check: ## Fail if Go or Rust code isn't formatted
	@cd $(GO_DIR) && out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	cd $(USSD_DIR) && cargo fmt --check

.PHONY: vet
vet: ## go vet the Go services
	cd $(GO_DIR) && go vet ./...

.PHONY: test
test: ## Run Go and Rust tests
	cd $(GO_DIR) && go test ./...
	cd $(USSD_DIR) && cargo test

.PHONY: caddy-check
caddy-check: ## Parse both Caddyfiles (needs Docker)
	@if command -v docker >/dev/null && docker info >/dev/null 2>&1; then \
		for f in Caddyfile Caddyfile.local; do \
			docker run --rm -v $(abspath gateway):/etc/caddy:ro $(CADDY_IMAGE) caddy adapt --config /etc/caddy/$$f >/dev/null && echo "$$f ok" || exit 1; \
		done; \
	else echo "skipping Caddyfile check (Docker isn't running)"; fi

.PHONY: compose-check
compose-check: ## Validate both compose files
	$(COMPOSE) -f docker-compose.yml config -q
	$(LOCAL) config -q
	@echo "compose files ok"

.PHONY: build
build: ## Build every image locally (same Dockerfiles CI uses)
	$(LOCAL) build

##@ Dependencies

.PHONY: bump-loom
bump-loom: ## Move to a loom release: make bump-loom v=v0.0.4
	@test -n "$(v)" || { echo "usage: make bump-loom v=<version>"; exit 1; }
	cd $(GO_DIR) && go get github.com/rhaqim/loom@$(v) \
		&& (go get github.com/rhaqim/loom/generator/anthropic@$(v) 2>/dev/null \
			|| { echo "no generator/anthropic/$(v) tag; using main"; go get github.com/rhaqim/loom/generator/anthropic@main; }) \
		&& go mod tidy && go build ./...

.PHONY: bump-ussd
bump-ussd: ## Move to a ussdframework commit: make bump-ussd rev=abc1234
	@test -n "$(rev)" || { echo "usage: make bump-ussd rev=<commit>"; exit 1; }
	cd $(USSD_DIR) && sed -i.bak -E 's/(ussdframework = \{[^}]*rev = ")[^"]+"/\1$(rev)"/' Cargo.toml && rm Cargo.toml.bak \
		&& cargo update -p ussdframework && cargo build

##@ Server (run from /opt/demo on the box; CI normally does this)

.PHONY: server-up
server-up: ## Pull and start images for a commit: make server-up TAG=<sha>
	@test -n "$(TAG)" || { echo "usage: make server-up TAG=<git sha or latest>"; exit 1; }
	TAG=$(TAG) $(COMPOSE) pull --quiet
	TAG=$(TAG) $(COMPOSE) up -d --remove-orphans
	docker image prune -f >/dev/null

.PHONY: server-logs
server-logs: ## Follow production logs (all, or one: make server-logs s=loom)
	$(COMPOSE) logs -f --tail 100 $(s)

.PHONY: backup
backup: ## Snapshot demo.db with SQLite's backup into ./backups/
	@mkdir -p backups
	docker run --rm -v demo_demo_data:/data -v $(abspath backups):/out alpine \
		sh -c 'apk add -q sqlite && sqlite3 /data/demo.db ".backup /out/demo-$$(date +%F-%H%M).db"'
	@ls -1t backups | head -1

##@ Operations

.PHONY: health
health: ## Check every service through the gateway (API=... to target another host)
	@for p in api/health api/ussd/health api/buckt/health api/loom/health; do \
		code=$$(curl -s -o /dev/null -w '%{http_code}' $(API)/$$p); \
		printf "  %-20s %s\n" $$p $$code; \
	done

.PHONY: feedback
feedback: ## Read the latest feedback (needs ADMIN_TOKEN in the environment)
	@test -n "$$ADMIN_TOKEN" || { echo "set ADMIN_TOKEN (see .env on the server)"; exit 1; }
	@curl -fsS -H "Authorization: Bearer $$ADMIN_TOKEN" $(API)/api/feedback | \
		(command -v jq >/dev/null && jq -r '.[] | "\(.created_at)  [\(.demo)] \(if .rating > 0 then "★" * .rating else "-" end)  \(.message)\(if .contact != "" then "  (" + .contact + ")" else "" end)"' || cat)

.PHONY: token
token: ## Generate a value for ADMIN_TOKEN
	@openssl rand -hex 32
