# Name of the application to build is taken from the directory.
NAME=$(shell basename $(CURDIR))

# with_postgres starts a temporary PostgreSQL 18 container, waits for it to
# accept connections, and exports STRATA_TEST_DATABASE_URL pointing at it for
# the shell commands that follow in the same recipe line. The container is
# removed when those commands finish, whether or not they succeed.
with_postgres = set -e; \
	name=$(NAME)-test-$$$$; \
	docker run --detach --rm --name $$name \
		--env POSTGRES_PASSWORD=strata --env POSTGRES_DB=strata_test \
		--publish 127.0.0.1::5432 postgres:18-alpine >/dev/null; \
	trap "docker rm --force $$name >/dev/null" EXIT; \
	port=$$(docker port $$name 5432/tcp | head -1 | sed 's/.*://'); \
	tries=0; \
	until docker exec $$name pg_isready --quiet --host 127.0.0.1 --username postgres; do \
		tries=$$((tries + 1)); \
		if [ $$tries -ge 30 ]; then echo 'postgres did not become ready' >&2; exit 1; fi; \
		sleep 1; \
	done; \
	export STRATA_TEST_DATABASE_URL=postgres://postgres:strata@127.0.0.1:$$port/strata_test;

.PHONY: help
help: ## Display valid Makefile targets.
	@echo 'targets:'
	@echo
	@grep -E '^[a-zA-Z_-]+:[[:space:]]*##[[:space:]]*.*$$' $(MAKEFILE_LIST) | sed -E 's/:[[:space:]]*##[[:space:]]*/#/' | column -s '#' -t

.PHONY: clean
clean: ## Clean up build artifacts
	@rm -rf coverage.out coverage.html $(NAME) data

.PHONY: update
update: ## Updates go module dependencies
	@go get -u -t ./...
	@go mod tidy

.PHONY: test
test: ## Run unit tests
	@go test ./...

.PHONY: test-integration
test-integration: ## Run unit and integration tests against a temporary PostgreSQL 18 container
	@$(with_postgres) go test -count=1 ./...

.PHONY: coverage
coverage: ## Run unit and integration tests with coverage analysis against a temporary PostgreSQL 18 container
	@$(with_postgres) go test -count=1 -covermode=atomic -coverprofile=coverage.out ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo 'open coverage.html for coverage heat map'
