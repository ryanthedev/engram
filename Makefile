# Engram build entry points. Tools run as version-pinned `go run` commands —
# no host installs required; CI uses these same targets.

REVIVE_VERSION := v1.12.0
OPENSEARCH_URL ?= http://localhost:9200

.PHONY: build test lint proto proto-check integration apply-templates eval dev-cluster

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
