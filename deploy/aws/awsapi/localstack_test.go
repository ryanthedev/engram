//go:build localstack

// These tests exercise the real SDKProvisioner against a running LocalStack
// (`make deploy-localstack-up` first, or `make deploy-localstack` which wraps
// the whole flow). They prove the -endpoint override genuinely reaches an
// AWS-compatible API and that the SDK translation for the Community-supported
// resource kinds (EC2/VPC, Secrets Manager) round-trips for real — not just
// against the in-memory fake.
//
// ECS and OpenSearch Service are LocalStack Pro features and are deliberately
// NOT tested here; their SDK paths are covered structurally by the fake-backed
// unit tests and remain a real-AWS manual-verification step.
package awsapi

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
)

func localstackProvisioner(t *testing.T) *SDKProvisioner {
	t.Helper()
	endpoint := os.Getenv("AWS_ENDPOINT_URL")
	if endpoint == "" {
		endpoint = "http://localhost:4566"
	}
	// LocalStack accepts any non-empty credentials; set them here so the run
	// does not depend on the caller's shell already exporting dummies.
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	ctx := context.Background()
	p, err := NewSDKProvisioner(ctx, config.WithBaseEndpoint(endpoint))
	if err != nil {
		t.Fatalf("building LocalStack provisioner at %s: %v", endpoint, err)
	}
	return p
}

// uniqueName keeps reruns from colliding without needing Math.random or a
// cleanup pass — LocalStack state is torn down with the container.
func uniqueName(prefix string) string {
	return prefix + "-" + time.Now().Format("150405.000000")
}

func TestLocalStack_VPC_DescribeCreateDescribe(t *testing.T) {
	p := localstackProvisioner(t)
	ctx := context.Background()
	name := uniqueName("engram-vpc")

	if _, ok, err := p.DescribeVPC(ctx, name); err != nil {
		t.Fatalf("DescribeVPC (pre-create) errored: %v", err)
	} else if ok {
		t.Fatalf("DescribeVPC reported a VPC named %q before creation", name)
	}

	created, err := p.CreateVPC(ctx, VPCSpec{Name: name, CIDR: "10.42.0.0/16"})
	if err != nil {
		t.Fatalf("CreateVPC: %v", err)
	}
	if created.ID == "" || created.CIDR != "10.42.0.0/16" {
		t.Fatalf("CreateVPC returned %+v, want a non-empty ID and the requested CIDR", created)
	}

	got, ok, err := p.DescribeVPC(ctx, name)
	if err != nil {
		t.Fatalf("DescribeVPC (post-create) errored: %v", err)
	}
	if !ok {
		t.Fatalf("DescribeVPC did not find the VPC %q just created", name)
	}
	if got.ID != created.ID {
		t.Fatalf("DescribeVPC returned ID %q, want the created %q", got.ID, created.ID)
	}
}

func TestLocalStack_Secret_DescribeCreateDescribe(t *testing.T) {
	p := localstackProvisioner(t)
	ctx := context.Background()
	name := uniqueName("engram-secret")

	if _, ok, err := p.DescribeSecret(ctx, name); err != nil {
		t.Fatalf("DescribeSecret (pre-create) errored: %v", err)
	} else if ok {
		t.Fatalf("DescribeSecret reported %q before creation", name)
	}

	created, err := p.CreateSecret(ctx, SecretSpec{Name: name, Value: "s3cr3t-value"})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if created.ARN == "" {
		t.Fatalf("CreateSecret returned an empty ARN")
	}

	got, ok, err := p.DescribeSecret(ctx, name)
	if err != nil {
		t.Fatalf("DescribeSecret (post-create) errored: %v", err)
	}
	if !ok {
		t.Fatalf("DescribeSecret did not find the secret %q just created", name)
	}
	if got.ARN == "" {
		t.Fatalf("DescribeSecret returned an empty ARN for %q", name)
	}
	// The state must never carry the plaintext value (the CreateSecret
	// contract) — SecretState has no value field, so this is enforced by type;
	// assert the metadata we do expect instead.
	if got.VersionID == "" {
		t.Fatalf("DescribeSecret returned no version id for %q", name)
	}
}
