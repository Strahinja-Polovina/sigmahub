# Turnkey staging/prod ops for the full SigmaHub stack (see staging.md).
# Usage: make -C cp/deploy up|logs|smoke|down|down-hard
COMPOSE = docker compose -f docker-compose.yml

.PHONY: up logs smoke down down-hard
up: ## Build + start CP + dashboard + Postgres (reads .env)
	$(COMPOSE) up -d --build

logs: ## Follow control-plane + dashboard logs
	$(COMPOSE) logs -f cp web

smoke: ## Verify the running stack end to end (needs CP_URL + CP_PROVISION_TOKEN)
	CP_URL="$(CP_URL)" CP_PROVISION_TOKEN="$(CP_PROVISION_TOKEN)" ./smoke.sh

down: ## Stop the stack, keep volumes (KMS key + data survive)
	$(COMPOSE) down

down-hard: ## Stop AND wipe volumes — deletes the KMS key. Throwaway staging only.
	$(COMPOSE) down -v
