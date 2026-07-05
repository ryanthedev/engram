# Runbook: Testing engram-deploy against LocalStack

Exercise the `engram-deploy` CLI against a local AWS-compatible API — no AWS
account, no credentials, no cost. This proves the deploy tooling's SDK paths
round-trip against a real API surface, not just the in-memory fake.

## TL;DR

```bash
make deploy-localstack        # boot LocalStack, run the tagged SDK tests, tear down
```

Expected: `TestLocalStack_VPC_*` and `TestLocalStack_Secret_*` PASS (real
round-trips against LocalStack), alongside the fake-backed converge/rollback
unit tests. Exit 0.

## What runs against a real API vs what doesn't

LocalStack **Community** (the free image used here) implements the resource
kinds engram-deploy uses for two of its four provisioners:

| Resource | Provisioner methods | LocalStack Community | Proven by |
|----------|--------------------|----------------------|-----------|
| EC2 / VPC | `DescribeVPC` / `CreateVPC` | ✅ supported | `TestLocalStack_VPC_DescribeCreateDescribe` (real create+describe) |
| Secrets Manager | `DescribeSecret` / `CreateSecret` | ✅ supported | `TestLocalStack_Secret_DescribeCreateDescribe` (real create+describe) |
| OpenSearch Service (managed domain) | `DescribeDomain` / `CreateDomain` | ❌ Pro only | fake-backed unit tests + real-AWS manual step |
| ECS | `DescribeService` / `CreateService` / … | ❌ Pro only | fake-backed unit tests + real-AWS manual step |

So a **full** `Converge` against Community LocalStack fails at the first
Pro-only resource. Converge describes the domain first, so the failure you see
is:

```
$ go run ./cmd/engram-deploy -env staging -image engram:v1 -endpoint http://localhost:4566
using AWS endpoint override: http://localhost:4566
error converging: ... opensearch DescribeDomain ...: StatusCode: 501,
  api error InternalFailure: Service 'opensearch' is not enabled ...
```

That is expected and correct — it confirms the `-endpoint` override reaches
LocalStack. The VPC and Secrets Manager SDK translations are verified directly
by the `localstack`-tagged tests rather than through a full converge.

To exercise the domain/ECS paths against a real API you need either LocalStack
Pro (`SERVICES=opensearch,ecs,...` with a Pro auth token) or a real AWS account
(the documented manual-verification step in the Phase-7 report).

## How the endpoint override works

`engram-deploy` takes `-endpoint <url>` (default `$AWS_ENDPOINT_URL`). When
set, it threads `config.WithBaseEndpoint` into `NewSDKProvisioner`, so every
service client targets that URL. `WithBaseEndpoint("")` is a no-op, so the
real-AWS path is unchanged when the flag is unset — the same binary reaches
real AWS with credentials or LocalStack without them.

## Manual steps

```bash
make deploy-localstack-up     # boot LocalStack on :4566, wait for health
# any deploy CLI invocation, pointed at LocalStack with dummy creds:
AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_REGION=us-east-1 \
  go run ./cmd/engram-deploy -env staging -image engram:v1 -endpoint http://localhost:4566
make deploy-localstack-down    # stop + remove the container and its volume
```

## Going to real AWS

Same binary, drop `-endpoint`, provide real credentials (any of the standard
aws-sdk-go-v2 sources):

```bash
aws configure sso && aws sso login   # or `aws configure` with an IAM key
AWS_PROFILE=<profile> make deploy-staging IMAGE=engram:<tag>
```

CI (`.github/workflows/deploy.yml`) uses GitHub OIDC → an IAM role, so no
static keys live in the repo. Real managed resources cost money (an OpenSearch
domain alone is ~$50+/mo) — see the Phase-7 report before running against a
billed account.
