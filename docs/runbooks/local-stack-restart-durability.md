# Runbook: Local stack does not come back after a restart (macOS)

**Symptom:** `memory_search` / `memory_status` fail from the MCP plugin, or enrichment sits at a fixed count forever. Nothing alerted, because none of this is the AWS deployment — it is the local podman stack on the dev Mac, and the live personal memory store lives in it (`engram-e2e-os` on `:9201`, see `docs/mcp.md`).

There are three independent things that must be up, and they fail differently:

| Layer | What it is | If it's down |
|---|---|---|
| The podman VM | `podman-machine-default` (applehv, 8 CPU / 16 GiB) | Every container is gone. `podman ps` errors or returns nothing. |
| The containers | `engram-e2e-os`, `local-engramd-1`, `local-embed-1`, `local-stub-llm-1` | Memory is unreachable. |
| The host embedder | `deploy/local/embed-real/run-host.sh`, native on the Mac's GPU | Enrichment stalls, and retrieval **silently** degrades to BM25 (see the `DefaultEmbedTimeout` note below). |

## Detection

```
podman machine list                                   # "Currently running"?
podman ps --format '{{.Names}}\t{{.Status}}'          # four local-* / engram-e2e-os, all healthy
curl -s http://127.0.0.1:8081/health                  # {"status":"ok","model":"BAAI/bge-m3","device":"mps"}
curl -s http://127.0.0.1:9201/_cluster/health         # green
launchctl print gui/$(id -u)/com.r.engram-embed | grep -E 'state|pid'
```

The embedder check is the one that fails quietly. A dead embedder does not make search *fail*, it makes search *worse* — BM25-only results with no error anywhere.

## What is in place

Two launchd user agents (they are not in this repo — they carry absolute `/Users/<you>` paths; the content is reproduced below) plus container restart policies.

**1. `~/Library/LaunchAgents/com.r.engram-embed.plist`** — keeps the host embedder alive.

```xml
<key>ProgramArguments</key>
<array><string>/Users/r/repos/engram/deploy/local/embed-real/run-host.sh</string></array>
<key>WorkingDirectory</key><string>/Users/r/repos/engram/deploy/local/embed-real</string>
<key>EnvironmentVariables</key>
<dict><key>PATH</key><string>/Users/r/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string></dict>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>ThrottleInterval</key><integer>30</integer>
```

- **PATH must be set explicitly.** launchd agents get a minimal PATH, and `run-host.sh` needs `uv` (`/opt/homebrew/bin`) to build the venv on a cold start.
- **`ThrottleInterval 30`** because `run-host.sh` hard-exits when torch reports no MPS device. That is a permanent failure, not a transient one, and `KeepAlive` would otherwise spin the log.
- Logs: `~/.config/services/logs/engram-embed.log`. A healthy cold start ends with `embed: BAAI/bge-m3 resident on mps, dim=1024`.

**2. `~/Library/LaunchAgents/com.r.podman-machine.plist`** — starts the VM at login.

```xml
<key>ProgramArguments</key>
<array><string>/opt/homebrew/bin/podman</string><string>machine</string><string>start</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><false/>
```

One-shot on purpose. `podman machine start` exits **125 / "already running"** when the VM is already up; with `KeepAlive` that benign case would look like a crash and respawn forever. Seeing `last exit code = 125` in `launchctl print` is normal.

**3. Container restart policies — must be `always`, not `unless-stopped`.**

```
podman update --restart always engram-e2e-os local-engramd-1 local-embed-1 local-stub-llm-1 engram-dev-os
```

This is the trap. What brings containers back after the VM restarts is `podman-restart.service` inside the VM, and it is:

```
ExecStart=/usr/bin/podman $LOGGING start --all --filter restart-policy=always
```

It matches **`always` only**. An `unless-stopped` container looks protected and silently does not come back. That service also ships **disabled**:

```
podman machine ssh sudo systemctl enable podman-restart.service
```

Tradeoff accepted: with `always`, a container you deliberately stopped comes back on the next VM boot.

## Remediation

```
podman machine start                                   # if the VM is down
podman start engram-e2e-os local-engramd-1 local-embed-1 local-stub-llm-1
launchctl kickstart -k gui/$(id -u)/com.r.engram-embed  # restart the embedder
```

`podman update --restart` changes policy in place — it does not recreate or bounce the container, so it is safe to run against the live memory store.

**Never** reach for `make e2e` / `make e2e-down` to fix this. `e2e-down` is `docker compose down -v`, and it destroys the live memory store's volume (`docs/mcp.md` carries the same warning).

## Known gaps

- **A dead embedder is invisible to retrieval.** `DefaultEmbedTimeout` is 50 ms (`internal/retrieval/opensearch.go`), sized for a co-located container. Measured through the host proxy: 20–23 ms warm, **53 ms cold** — already over budget. When it blows, retrieval falls back to BM25 with no signal. The ~10 s launchd respawn window is exactly this case.
- **A VM that dies mid-session stays dead.** `com.r.podman-machine` is login-only by design.
- **`engram-dev-os` was OOM-killed once** (`exit 137`, `OOMKilled: true`) with no container memory limit — it died from VM-level pressure while the VM had 4 GB and an in-container CPU embedder was competing for it. If it recurs, check VM headroom (`podman machine ssh free -h`) before blaming the container.
