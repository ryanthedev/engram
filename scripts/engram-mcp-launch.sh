#!/usr/bin/env bash
# Plugin entrypoint for the engram MCP server (.claude-plugin/plugin.json).
#
# bin/ is gitignored, so a marketplace install (a clone of this repo with no
# build step) has no binary at ${CLAUDE_PLUGIN_ROOT}/bin. This resolves one:
#   1. ${plugin root}/bin/engram-mcp — a dev checkout loaded with --plugin-dir
#      after `go build -o bin/engram-mcp ./cmd/engram-mcp`.
#   2. engram-mcp on PATH, then the usual `go install` targets — a machine
#      that installed it with:
#        GOBIN=~/.local/bin go install github.com/ryanthedev/engram/cmd/engram-mcp@latest
# Arguments pass through unchanged.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
for candidate in "$root/bin/engram-mcp" "$(command -v engram-mcp || true)" \
	"$HOME/.local/bin/engram-mcp" "$HOME/go/bin/engram-mcp"; do
	if [[ -n "$candidate" && -x "$candidate" ]]; then
		exec "$candidate" "$@"
	fi
done

echo "engram-mcp not found. Install it with:" >&2
echo "  GOBIN=~/.local/bin go install github.com/ryanthedev/engram/cmd/engram-mcp@latest" >&2
exit 127
