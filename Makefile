# FirmScout developer tasks.
#
# Every target delegates to scripts/dev.sh, so the same commands work whether you prefer
# make or running the script directly, and there is only one place to change them.

.DEFAULT_GOAL := help
.PHONY: help up down logs migrate test lint fmt check generate mermaid schemas

help: ## Show this help
	@echo "FirmScout"
	@echo
	@grep -E '^[a-z-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "Run 'make check' before opening a pull request."

up: ## Build and start the stack (PostgreSQL, API, worker, website)
	@bash scripts/dev.sh up

down: ## Stop the stack
	@bash scripts/dev.sh down

logs: ## Follow the stack's logs
	@bash scripts/dev.sh logs

migrate: ## Apply pending database migrations
	@bash scripts/dev.sh migrate

test: ## Run the Go test suite
	@bash scripts/dev.sh test

lint: ## Run golangci-lint
	@bash scripts/dev.sh lint

fmt: ## Format Go code
	@bash scripts/dev.sh fmt

generate: ## Regenerate sqlc code
	@bash scripts/dev.sh generate

mermaid: ## Validate every Mermaid diagram
	@bash scripts/dev.sh mermaid

schemas: ## Validate the registry against its JSON Schemas
	@bash scripts/dev.sh schemas

check: ## Run every check, in order
	@bash scripts/dev.sh check
