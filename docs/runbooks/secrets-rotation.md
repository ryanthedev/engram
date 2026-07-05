# Runbook: Secrets rotation

The procedure `cmd/engram-deploy` and `deploy/aws/awsapi/converge.go` point to when they say a secret's value is "provisioned/rotated out of band, never checked into this repo." Not one of the five DW-7.7 incident runbooks — a referenced operational procedure.

## Why Converge never touches secret values

`awsapi.Converge` only ever **creates a missing** secret (with a placeholder value — see `cmd/engram-deploy/environments.go`, which commits `REPLACE_VIA_ROTATION_RUNBOOK`, never a real key). It never diffs, re-reads, re-logs, or updates an existing secret's value. This is deliberate: the deploy path must never have a real secret value in memory or in a log line, and rotation is a reviewed, deliberate action, not a side effect of a routine converge. So after the first deploy creates the Secrets Manager entries, an operator sets and rotates their real values through the steps below — outside the deploy CLI.

## Secrets managed

Per environment (`engram-<env>/...`), created by Converge as placeholders:
- `engram-<env>/extract-api-key` — the extraction LLM endpoint credential.
- `engram-<env>/gate-api-key` — the experience write-gate judge credential.

The ECS task definitions reference these by ARN (a real Fargate wiring step — not baked into the minimal `ServiceSpec` here; see the SDKProvisioner note in `deploy/aws/awsapi/sdk.go`), so a rotation is picked up on the next task placement.

## Initial provisioning (first deploy)

1. Converge has created the secret with the placeholder value.
2. Set the real value out of band (never via a committed file, never echoed to a shared log):
   ```
   aws secretsmanager put-secret-value \
     --secret-id engram-<env>/extract-api-key \
     --secret-string "$REAL_KEY"   # from your secret store / password manager, not the repo
   ```
3. Force a new task placement so services pick up the value:
   ```
   aws ecs update-service --cluster engram-<env> --service worker --force-new-deployment
   ```

## Routine rotation

1. Obtain the new credential from the provider.
2. `put-secret-value` as above — Secrets Manager versions it automatically (the old version stays until superseded, so an in-flight task is never left credential-less mid-rotation).
3. Force a new deployment of the services that read the secret (`worker` for extraction, `engramd`/`worker` for the gate depending on wiring) so running tasks re-read it.
4. Confirm from telemetry that extraction/gate calls still succeed post-rotation (`engram_gate_*` verdict rates continue, extraction cost gauge continues to move) — a rotation that installed a bad key shows up as a spike in gate errors / a stalled worker (see `docs/runbooks/01-worker-down-or-lagging.md`).

## No secret ever in the repo or logs

The repo only ever contains the literal placeholder `REPLACE_VIA_ROTATION_RUNBOOK`. `cmd/engram-deploy/environments_test.go`'s `TestEnvironment_SecretsNeverCarryReadableProductionValues` fails CI if a real-looking value is ever committed into the environment definitions.

## Not exercised here

No real AWS Secrets Manager in this build environment — the `put-secret-value`/`force-new-deployment` steps are a documented manual procedure for an operator with real account access, consistent with the rest of the DW-7.1 real-AWS residue.
