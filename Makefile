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

.PHONY: build test lint proto proto-check integration apply-templates eval dev-cluster e2e e2e-up e2e-down \
	deploy-staging deploy-prod deploy-localstack deploy-localstack-up deploy-localstack-down \
	loadtest drill e2e-cloud eval-seed eval-gate eval-dashboard eval-drill

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
# (pinned 3.1) — includes the Phase-2 worker/outbox/ledger live tests and the
# Phase-7 versioned-index/alias-flip mechanism (deploy/aws/reindex).
integration:
	ENGRAM_OPENSEARCH_URL=$(OPENSEARCH_URL) go test -tags=integration -count=1 -v ./internal/spike/ ./internal/store/ ./internal/retrieval/ ./internal/server/ ./internal/eval/... ./internal/worker/ ./internal/ingest/ ./internal/experience/ ./internal/graph/ ./internal/telemetry/ ./deploy/aws/reindex/...

# DW-1.5: performance harness (perf environment, not CI — see the plan).
perf:
	go run ./cmd/engram-perf

# DW-7.2: the 10x-S1-sustained + 5x-burst load test (measure-first — this is
# the measurement the phase's SLO/RAM evidence is built on, not a tuning
# pass). Run against the local dev cluster; see cmd/engram-loadtest's doc
# comment for flags (seed corpus size, rates, phase durations).
loadtest:
	go run ./cmd/engram-loadtest -url $(OPENSEARCH_URL)

# DW-7.4 / DW-7.5: the restore and failure drills — real exercises against
# the local pinned-3.1 dev cluster (stop/restart its container) and a real
# engram-server process (kill/restart it). Destructive to the dev cluster's
# current state (scratch-indexed, but briefly stops the container) — never
# run this against a shared cluster other work depends on concurrently.
# Requires `make dev-cluster` first (path.repo is set there for the restore
# drill's snapshot repository).
drill:
	ENGRAM_OPENSEARCH_URL=$(OPENSEARCH_URL) go test -tags=drill -count=1 -v ./e2e/cloud/...

# DW-7.1: the e2e "cloud profile" — the SAME e2e suite (e2e/) run in external
# mode against a reachable staging environment instead of self-hosting a
# local stack (e2e/harness.go's Boot() already supports this via
# ENGRAM_E2E_ADDR). Requires network reachability to staging's gRPC endpoint
# and OpenSearch domain (e.g. via VPN/bastion in a real deployment) — set
# both env vars before invoking this target; there is no default staging
# address baked in.
e2e-cloud:
	go test -tags=e2e -count=1 -timeout=300s ./e2e/

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

# DW-7.1: converge staging/prod via the idempotent Go deploy CLI (D24 — no
# Terraform/HCL). IMAGE must be set to the image tag to deploy; requires
# real AWS credentials (standard aws-sdk-go-v2 resolution) — there are none
# in this build environment, so these targets are exercised for real only in
# CI (.github/workflows/deploy.yml) or by an operator with staging/prod
# access. Re-running with the same IMAGE is a verified no-op (see
# deploy/aws/awsapi's idempotency tests for the proof against a fake
# Provisioner backing the identical Converge logic).
deploy-staging:
	go run ./cmd/engram-deploy -env staging -image $(IMAGE)

deploy-prod:
	go run ./cmd/engram-deploy -env prod -image $(IMAGE)

# LocalStack: exercise the deploy CLI against a local AWS-compatible API — no
# AWS account or credentials needed. Community LocalStack implements the VPC +
# Secrets Manager paths; ECS + OpenSearch domain are Pro-only, so a full
# `deploy-localstack` converge succeeds on the VPC/Secret and reports the
# domain/services as failing by design (see docs/runbooks/localstack-deploy.md).
LOCALSTACK_ENDPOINT ?= http://localhost:4566
LS_ENV = AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_REGION=us-east-1 AWS_ENDPOINT_URL=$(LOCALSTACK_ENDPOINT)

deploy-localstack-up:
	podman compose -f deploy/aws/localstack-compose.yml up -d
	@echo "waiting for LocalStack health at $(LOCALSTACK_ENDPOINT) ..."
	@for i in $$(seq 1 30); do \
		curl -sf $(LOCALSTACK_ENDPOINT)/_localstack/health >/dev/null 2>&1 && { echo "LocalStack ready"; exit 0; }; \
		sleep 2; \
	done; echo "LocalStack did not become healthy in time" >&2; exit 1

deploy-localstack-down:
	podman compose -f deploy/aws/localstack-compose.yml down -v

# The real-SDK-against-LocalStack integration test (VPC + Secrets Manager
# round-trips). Boots LocalStack, runs the `localstack`-tagged tests, tears down.
deploy-localstack: deploy-localstack-up
	$(LS_ENV) go test -tags=localstack -count=1 -v ./deploy/aws/awsapi/ ; \
		status=$$? ; $(MAKE) deploy-localstack-down ; exit $$status

# Phase 8 (D9): release gates. `eval-seed` is the ONLY write-bearing step —
# idempotent by fixed doc ids, safe to re-run — so `eval-gate` (what
# .github/workflows/deploy.yml's `gates` job actually calls) stays strictly
# read-only against its target, per the phase's constraint. Against a
# throwaway/ephemeral OpenSearch (e.g. ci.yml's `integration` job, which runs
# the whole ./internal/eval/... package unfiltered), running `eval-seed`
# first is required; against a persistent environment that already has the
# fixture corpus, `eval-gate` alone suffices.
eval-seed:
	ENGRAM_OPENSEARCH_URL=$(OPENSEARCH_URL) go test -tags=integration -count=1 -v -run '^TestSeedEvalFixtures$$' ./internal/eval/gate/...

# DW-8.1/8.2/8.3: the three independent release gates (hallucination,
# retrieval-regression, experience-following). Exits non-zero on any
# threshold breach (or flaky-gate quarantine) — that exit code is what
# blocks `deploy-prod` in deploy.yml's `gates` job. Typically completes in
# low single-digit seconds (well inside the ≤15 min budget, DW-8.5) since it
# only reads already-seeded data plus small local threshold/trend files.
eval-gate:
	ENGRAM_OPENSEARCH_URL=$(OPENSEARCH_URL) go test -tags=integration -count=1 -v -run '^TestGate_' ./internal/eval/gate/...

# DW-8.6: regenerate docs/eval/dashboard.md from the accumulated
# eval/gate-runs/history.jsonl trend log, with no new gate run. eval-gate and
# eval-drill already regenerate it as a side effect of every run; this
# target is for a manual refresh (e.g. immediately after a baseline
# re-record).
eval-dashboard:
	go test -tags=integration -count=1 -v -run '^TestRenderDashboardOnly$$' ./internal/eval/gate/...

# DW-8.4: the bad-release drill, opt-in only (ENGRAM_EVAL_DRILL gates the
# e2e-level drill; the internal/eval/gate one always runs standalone here
# since it owns its own dedicated, disposable scratch index and never
# touches staging). Never wired into ci.yml or deploy.yml automatically —
# same precedent as `make drill` (Phase 7): a deliberately-invoked,
# adversarial exercise, not a routine gate. Requires `make dev-cluster`.
eval-drill:
	ENGRAM_OPENSEARCH_URL=$(OPENSEARCH_URL) go test -tags=integration -count=1 -v -run TestDrillBadRelease_HallucinationGateBlocks ./internal/eval/gate/...
	ENGRAM_EVAL_DRILL=1 go test -tags=e2e -count=1 -v -run TestBadReleaseDrill ./e2e/
