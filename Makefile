# Engram build entry points. Tools run as version-pinned `go run` commands —
# no host installs required; CI uses these same targets.

REVIVE_VERSION := v1.12.0
OPENSEARCH_URL ?= http://localhost:9200

# Container runtime for the local e2e stack: podman when present, else docker.
COMPOSE ?= $(shell command -v podman >/dev/null 2>&1 && echo "podman compose" || echo "docker compose")
COMPOSE_FILE := deploy/local/docker-compose.yml
# Host endpoints the compose stack exposes (distinct from the dev cluster).
E2E_OS_URL := http://localhost:9201
E2E_ADDR := localhost:7071

.PHONY: build test lint proto proto-check integration apply-templates eval dev-cluster e2e e2e-up e2e-down

build:
	go build ./...

test:
	go test ./...

# DW-0.2: revive's exported rule enforces doc-comment contracts.
# api/engrampb is machine-generated (buf) and excluded.
lint:
	go vet ./...
	go run github.com/mgechev/revive@$(REVIVE_VERSION) -config revive.toml -set_exit_status -exclude ./api/engrampb/... ./...

# DW-0.7: regenerate gRPC/protobuf code from api/proto.
proto:
	./scripts/codegen.sh

# CI guard: generated code must match the checked-in proto.
proto-check: proto
	git diff --exit-code -- api/engrampb

# DW-0.4 / DW-0.9 / DW-1.x / DW-2.x: live-cluster integration + spike tests
# (pinned 3.1) — includes the Phase-2 worker/outbox/ledger live tests.
integration:
	ENGRAM_OPENSEARCH_URL=$(OPENSEARCH_URL) go test -tags=integration -count=1 -v ./internal/spike/ ./internal/store/ ./internal/retrieval/ ./internal/server/ ./internal/eval/... ./internal/worker/ ./internal/ingest/

# DW-1.5: performance harness (perf environment, not CI — see the plan).
perf:
	go run ./cmd/engram-perf

apply-templates:
	go run ./cmd/engram-apply-templates -url $(OPENSEARCH_URL)

eval:
	go run ./cmd/engram-eval

dev-cluster:
	./scripts/dev-cluster.sh

# DW-3.1: boot the full local stack (OpenSearch 3.1 + embedding server + stub
# LLM + engramd) from a clean checkout and run the e2e loop through MCP, the
# CLI, and gRPC. The stack runs in containers; the e2e harness connects to it
# via ENGRAM_E2E_ADDR (external mode) and drives the CLI/MCP client binaries.
e2e: e2e-up
	@echo "running e2e suite against the compose stack..."
	@ENGRAM_OPENSEARCH_URL=$(E2E_OS_URL) ENGRAM_E2E_ADDR=$(E2E_ADDR) \
		go test -tags=e2e -count=1 -timeout=300s ./e2e/ ; \
		status=$$? ; \
		$(MAKE) e2e-down ; \
		exit $$status

# e2e-up builds the images and starts the stack, blocking until every service
# is healthy (compose --wait honors the healthchecks / depends_on gating).
e2e-up:
	$(COMPOSE) -f $(COMPOSE_FILE) up -d --build --wait

# e2e-down tears the stack down and removes its volumes.
e2e-down:
	$(COMPOSE) -f $(COMPOSE_FILE) down -v
