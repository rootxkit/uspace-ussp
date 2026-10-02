# uspace-ussp developer targets. CI (.github/workflows/ci.yml) runs the
# same commands. On Windows set GOROOT and GO, for example:
#   make test GO=/c/Users/<you>/AppData/Local/anaconda3/bin/go
#
# bash, not /bin/sh: the recipes use pipefail, which ubuntu's dash lacks.
SHELL := bash
GO    ?= go
PKGS  ?= ./...

# Tool versions, pinned here and mirrored in .github/workflows/ci.yml;
# change both in one `ci:` commit. The linters equal uspace-core's.
# oapi-codegen is not pinned here: it is the go.mod tool directive.
GOLANGCI_LINT_VERSION ?= v2.14.0
STATICCHECK_VERSION   ?= v0.8.1
GOVULNCHECK_VERSION   ?= v1.8.0
# The gitleaks version gitleaks-action runs in CI (GITLEAKS_VERSION there).
GITLEAKS_VERSION      ?= v8.24.3

COMPOSE_FILE = deploy/compose/docker-compose.yml
COMPOSE_ENV  = deploy/compose/.env
COMPOSE      = docker compose --env-file $(COMPOSE_ENV) -f $(COMPOSE_FILE)
PROJECT      = uspace-ussp
IMAGE        ?= ghcr.io/rootxkit/uspace-ussp
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT       ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)

.PHONY: all build vet fmt fmt-check tools staticcheck lint tidy test race cover \
        integration generate fetch-standards check-generated check-schemas check-deps check-hostnames \
        check-contracts secrets vulncheck image compose-deps compose-up compose-down \
        conformance ci clean migrate-up migrate-down migrate-status

all: ci

build:
	$(GO) build $(PKGS)

vet:
	$(GO) vet $(PKGS)
	$(GO) vet -tags integration ./test/... ./internal/bus/... ./internal/app/tsdbwriter/...

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

tools:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	$(GO) install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
	$(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	$(GO) install github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION)

staticcheck:
	$(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) $(PKGS)

# Refuses to run a golangci-lint other than the pinned one: a different
# version enables different checks and would pass here but fail in CI.
lint: fmt-check vet staticcheck
	@v="v$$(golangci-lint version --short 2>/dev/null)"; \
	if [ "$$v" != "$(GOLANGCI_LINT_VERSION)" ]; then \
	  echo "golangci-lint $$v found, CI runs $(GOLANGCI_LINT_VERSION): run 'make tools'"; exit 1; fi
	golangci-lint run $(PKGS)

tidy:
	$(GO) mod tidy
	git diff --exit-code -- go.mod go.sum

test:
	$(GO) test -count=1 -shuffle=on $(PKGS)

# The race detector needs cgo (and a C toolchain) for the test binary
# only; the shipped binaries stay CGO_ENABLED=0.
race:
	CGO_ENABLED=1 $(GO) test -race -count=1 -shuffle=on $(PKGS)

cover:
	$(GO) test -count=1 -shuffle=on -coverprofile=coverage.out -covermode=atomic $(PKGS)
	$(GO) tool cover -func=coverage.out | tail -n 1

# Real PostgreSQL + PostGIS, TimescaleDB and NATS JetStream. The
# USSP_TEST_* URLs come from the environment (CI) or, when unset, from
# the stack of `make compose-deps` and the passwords in $(COMPOSE_ENV).
# Fails when a test fails and when zero tests ran: a suite that ran
# nothing proves nothing.
integration:
	@set -a; if [ -f $(COMPOSE_ENV) ]; then . ./$(COMPOSE_ENV); fi; set +a; \
	export USSP_TEST_PG_URL="$${USSP_TEST_PG_URL:-postgres://ussp_api:$${USSP_PG_API_PASSWORD}@127.0.0.1:57432/ussp_relational?sslmode=disable}"; \
	export USSP_TEST_TS_URL="$${USSP_TEST_TS_URL:-postgres://ussp_api:$${USSP_PG_API_PASSWORD}@127.0.0.1:57432/ussp_timeseries?sslmode=disable}"; 	export USSP_TEST_TS_OWNER_URL="$${USSP_TEST_TS_OWNER_URL:-postgres://ussp_tsdb:$${USSP_PG_TSDB_PASSWORD}@127.0.0.1:57432/ussp_timeseries?sslmode=disable}"; \
	export USSP_TEST_NATS_URL="$${USSP_TEST_NATS_URL:-nats://api:$${USSP_NATS_API_PASSWORD}@127.0.0.1:57422}"; \
	set -o pipefail; \
	$(GO) test -tags integration -count=1 -p 1 -v ./test/integration/... ./internal/bus/... ./internal/app/tsdbwriter/... 2>&1 | tee integration.log; \
	n=$$(grep -c '^--- PASS' integration.log || true); \
	echo "integration: $$n top-level tests passed"; \
	if [ "$$n" -eq 0 ]; then echo "integration: zero tests ran"; exit 1; fi

# oapi-codegen (scripts/generate.sh) and openapi-typescript (web/).
generate:
	GO=$(GO) scripts/generate.sh
	cd web && pnpm gen:api

# Re-fetches the pinned standard files and compares them (network;
# never in CI, which runs scripts/check-standards.sh offline).
fetch-standards:
	scripts/fetch-standards.sh -check

# The committed generated Go files are what the sources produce, and
# the pinned standard files are the ones api/standards/SOURCE and
# uspace-core record (offline; the web types are checked by the web job).
check-generated:
	GO=$(GO) scripts/check-generated.sh

check-schemas:
	scripts/check-schemas.sh

check-deps:
	GO=$(GO) scripts/check-deps.sh

check-hostnames:
	scripts/check-hostnames.sh

check-contracts:
	scripts/check-contracts.sh

# Secret scan of the history and of every file that could be committed
# (tracked or untracked, not git-ignored), with .gitleaks.toml.
secrets:
	gitleaks detect --no-banner --redact
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	git ls-files -z -co --exclude-standard | tar --null -T - -cf - | tar -xf - -C "$$tmp"; \
	gitleaks detect --no-banner --redact --no-git --source "$$tmp"

# Known vulnerabilities the code can reach (symbol level).
vulncheck:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) $(PKGS)

image:
	docker build -f deploy/Dockerfile --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE):$(VERSION) .
	docker build -f deploy/web.Dockerfile -t $(IMAGE)-web:$(VERSION) web

# The first run writes random passwords to $(COMPOSE_ENV) (git-ignored).
$(COMPOSE_ENV):
	@rnd() { od -An -tx1 -N16 /dev/urandom | tr -d ' \n'; }; \
	grep -v '^#' deploy/compose/.env.example | grep '=' | while IFS== read -r k _; do echo "$$k=$$(rnd)"; done > $@
	@echo "wrote $@ (random local passwords)"

# The databases and NATS only, for `make integration` on a laptop.
compose-deps: $(COMPOSE_ENV)
	$(COMPOSE) up -d --wait timescaledb nats

compose-up: $(COMPOSE_ENV)
	USSP_VERSION=$(VERSION) USSP_COMMIT=$(COMMIT) $(COMPOSE) up -d --build --wait
	$(COMPOSE) ps --format 'table {{.Service}}\t{{.Status}}'

# Removes containers, volumes and the network, then checks that nothing
# of the project is left and says so (E-02: the success path of
# teardown is verified, not assumed).
compose-down:
	@if [ -f $(COMPOSE_ENV) ]; then $(COMPOSE) down -v --remove-orphans; \
	else docker compose -p $(PROJECT) down -v --remove-orphans; fi
	@left="$$(docker ps -aq --filter label=com.docker.compose.project=$(PROJECT))$$(docker volume ls -q --filter label=com.docker.compose.project=$(PROJECT))$$(docker network ls -q --filter name=^$(PROJECT)$$)"; \
	if [ -n "$$left" ]; then echo "compose-down: $(PROJECT) left containers, volumes or networks behind:"; \
	  docker ps -a --filter label=com.docker.compose.project=$(PROJECT); docker volume ls --filter label=com.docker.compose.project=$(PROJECT); \
	  docker network ls --filter name=^$(PROJECT)$$; exit 1; fi; \
	echo "compose-down: no container, volume or network of $(PROJECT) left"

# Both migration trees through the migrate subcommands (scripts/migrate.sh),
# against the stack of `make compose-deps` unless MIGRATE_PG_URL /
# MIGRATE_TS_URL name other databases. The relational tree runs as
# ussp_api, the time-series tree as ussp_tsdb (the owners). Down rolls
# each tree back one migration.
MIGRATE_ENV = set -a; if [ -f $(COMPOSE_ENV) ]; then . ./$(COMPOSE_ENV); fi; set +a; 	rel="$${MIGRATE_PG_URL:-postgres://ussp_api:$${USSP_PG_API_PASSWORD}@127.0.0.1:57432/ussp_relational?sslmode=disable}"; 	ts="$${MIGRATE_TS_URL:-postgres://ussp_tsdb:$${USSP_PG_TSDB_PASSWORD}@127.0.0.1:57432/ussp_timeseries?sslmode=disable}"

migrate-up:
	@$(MIGRATE_ENV); GO=$(GO) scripts/migrate.sh relational up "$$rel" && GO=$(GO) scripts/migrate.sh timeseries up "$$ts"

migrate-down:
	@$(MIGRATE_ENV); GO=$(GO) scripts/migrate.sh timeseries down "$$ts" && GO=$(GO) scripts/migrate.sh relational down "$$rel"

migrate-status:
	@$(MIGRATE_ENV); GO=$(GO) scripts/migrate.sh relational status "$$rel" && GO=$(GO) scripts/migrate.sh timeseries status "$$ts"

# The InterUSS DSS and uss_qualifier against this USSP: WP-19.
conformance:
	@echo "conformance: not implemented until WP-19 (deploy/conformance/)"; exit 1

ci: build lint race check-generated check-schemas check-deps check-hostnames check-contracts vulncheck secrets integration

clean:
	rm -f coverage.out integration.log
