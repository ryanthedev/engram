# Engram Code Standards

Greenfield (no code yet). These are the **intended** Go conventions; `/code-foundations:build`
consumes this file. Re-generate by scanning once real code lands.

## Language & layout
- Go 1.23+, module `github.com/ryanthedev/engram`.
- Standard layout: `cmd/` entrypoints, `internal/` for non-exported packages, `api/` for contracts.
- One package per bounded concern (`memory`, `retrieval`, `ingest`, `acl`, `graph`, `store`, `api`).

## Errors
- Return errors; never `panic` in library code. Wrap with `fmt.Errorf("...: %w", err)`.
- Sentinel errors for control flow (`errors.Is`/`errors.As`).

## Context & concurrency
- `context.Context` is the first param on every I/O and RPC call; honor cancellation.
- No goroutine without a clear lifecycle owner; use `errgroup` for fan-out.
- **All writes to shared memory use OpenSearch optimistic concurrency** (`if_seq_no`/`if_primary_term`).

## Interfaces & modules (APOSD)
- Deep modules: small interfaces over substantial implementations. `Store`, `Retriever`,
  `Extractor`, `Reconciler` are interfaces; OpenSearch is one implementation behind them.
- Define interfaces at the consumer; keep OpenSearch/vendor types out of public signatures.

## Testing
- Table-driven tests. Integration tests behind a build tag, run against a disposable OpenSearch
  (testcontainers).
- Every code-touching phase ships at least one error-path ("dirty") test.

## Observability
- Structured logs (`log/slog`); trace IDs propagated via `context`.
- Metrics on write/extract/retrieve latency and **extraction cost** (the dominant cost line).
