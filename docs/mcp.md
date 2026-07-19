# Engram MCP server — registration & round-trip

`engram-mcp` exposes Engram's memory to any MCP-capable agent host (Claude
Code, etc.) over stdio JSON-RPC 2.0. It maps three tools onto the engramd gRPC
API:

| Tool | Arguments | Returns |
|------|-----------|---------|
| `memory_ingest` | `event_id` (req), `text` (req), `source` | `{ id }` |
| `memory_search` | `query` (req), `k`, and the optional filters `kind`, `subject`, `predicate`, `object`, `extractor_version`, `since`, `until`, `include_superseded`, `sources` | `{ hits[] }` |
| `memory_status` | — | `{ healthy, tenant_id, user_id, agent_id, episodic_count, semantic_count, opensearch_version }` |

Every call is authenticated by a bearer token (`ENGRAM_TOKEN`), which binds the
session to a `(tenant_id, user_id, agent_id)` identity. Logs go to stderr;
stdout carries only the JSON-RPC stream.

## 1. Boot the stack

```bash
make e2e-up            # OpenSearch 3.1 + embedding server + stub LLM + engramd
# engramd gRPC is exposed on localhost:7071, OpenSearch on localhost:9201
```

> ⚠️ **Destructive teardown — never run `make e2e` or `make e2e-down` against a
> stack holding memories you care about.** Both run `docker compose … down -v`,
> which **deletes the OpenSearch volume** and every memory in it. This is the
> same compose stack (`engram-e2e-os`, :9201) you'd use to hold a live personal
> store — tearing it down wipes that store irrecoverably. `make e2e` also
> invokes `e2e-down` automatically on exit. To stop the stack **without** losing
> data, run `docker compose -f deploy/local/docker-compose.yml down` (no `-v`),
> or just leave it running. Reserve the `-v` teardown for a throwaway stack.

For a production deployment, point at the real engramd address instead.

## 2. Mint a token

```bash
go run ./cmd/engram token create \
  --tenant acme --user alice --agent claude \
  --url http://localhost:9201 --ttl 720h
# prints the raw token exactly once — copy it:
#   token created (handle 2a1add33a88c2c9f) — copy the raw token now, ...
#   egm_XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX
export ENGRAM_TOKEN=egm_XXXX...
```

## 3. Register with Claude Code

```bash
claude mcp add engram -- \
  env ENGRAM_TOKEN=$ENGRAM_TOKEN engram-mcp -addr localhost:7071
```

(Use the absolute path to the built `engram-mcp` binary, or `go run
./cmd/engram-mcp`, if it is not on `PATH`.) Claude Code then spawns the server
on demand and lists `memory_ingest`, `memory_search`, and `memory_status`.

## 4. Round-trip by hand (reference-client protocol)

The server speaks newline-delimited JSON-RPC on stdio. A manual round-trip:

```bash
export ENGRAM_TOKEN=egm_XXXX...
{
  echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}'
  echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
  echo '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory_ingest","arguments":{"event_id":"demo-1","text":"fact: alice | prefers | dark-mode"}}}'
  sleep 3   # let the async worker extract + reconcile
  echo '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"memory_search","arguments":{"query":"alice prefers dark-mode","k":5}}}'
} | engram-mcp -addr localhost:7071
```

Expected: `initialize` returns the protocol version and tools capability;
`tools/list` returns the three tools; the `memory_ingest` call returns an id;
the `memory_search` call returns a hit whose `fields_json` contains
`dark-mode`.

## 5. Live Claude Code round-trip (manual verification step)

The conformance of `initialize` / `tools/list` / `tools/call` is covered by
automated tests (`internal/mcp` conformance suite and the `e2e` full-loop test
that drives a spawned `engram-mcp` subprocess over real stdio). Demonstrating a
**live** Claude Code session cannot be automated from inside the build — it
requires an interactive Claude Code instance. To verify manually:

1. Run steps 1–3 above.
2. In a Claude Code session, ask the agent to "remember that alice prefers dark
   mode" (drives `memory_ingest`), then in a new turn ask "what does alice
   prefer?" (drives `memory_search`).
3. Confirm the second turn retrieves the fact.

This step is surfaced as a manual verification item; everything mechanically
testable is exercised by the automated suites.
