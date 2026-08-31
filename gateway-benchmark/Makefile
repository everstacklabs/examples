# AI gateway benchmark. See README.md and METHODOLOGY.md.

COMPOSE = docker compose -f compose/docker-compose.yml

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: up
up: ## Start the mock upstream and every public competitor gateway
	@$(COMPOSE) up -d --build
	@echo "waiting for containers to settle..."
	@sleep 10
	@$(MAKE) validate

.PHONY: up-subject
up-subject: ## Also start Everstack (requires EVERSTACK_IMAGE; see README)
	@$(COMPOSE) --profile subject up -d --build
	@sleep 10
	@$(MAKE) validate

.PHONY: down
down: ## Tear the stack down
	@$(COMPOSE) --profile subject down -v

.PHONY: validate
validate: ## Check every target answers before spending a full run on it
	@go run ./cmd/gwbench validate

.PHONY: bench
bench: ## Run the full suite: performance, correctness, report
	@go run ./cmd/gwbench run

.PHONY: perf
perf: ## Performance phases only
	@go run ./cmd/gwbench perf

.PHONY: checks
checks: ## Correctness scenarios only (fast; no load phases)
	@go run ./cmd/gwbench checks

.PHONY: matrix
matrix: ## Validate the capability matrix and print coverage and scores
	@go run ./cmd/gwbench matrix

.PHONY: report
report: ## Regenerate the markdown report from the last saved run
	@go run ./cmd/gwbench report

.PHONY: upstream
upstream: ## Run just the mock upstream locally (no docker needed)
	@go run ./cmd/mockupstream -addr :9800

.PHONY: links
links: ## Check every capability-matrix evidence URL still resolves
	@grep -oE 'evidence: "https://[^"]+"' matrix/capabilities.yaml \
		| sed 's/evidence: "//; s/"$$//' | sort -u \
		| while read -r u; do \
			code=$$(curl -sL -o /dev/null -w '%{http_code}' --max-time 12 "$$u"); \
			[ "$$code" = "200" ] || echo "$$code $$u"; \
		done; \
		echo "link check done (only non-200s are listed)"

.PHONY: test
test: ## Unit and integration tests for the harness itself
	@go test ./...
