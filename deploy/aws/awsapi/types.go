// Package awsapi is the Provisioner port (D24): a small, domain-shaped
// interface over the four AWS resource kinds engram-deploy manages (an
// OpenSearch Service domain, a VPC, Secrets Manager secrets, and ECS
// services). cmd/engram-deploy drives Converge/Rollback against this
// interface — never against the AWS SDK types directly — so the
// converge/idempotency/rollback/blue-green logic is unit-testable with
// FakeProvisioner without any AWS credentials, while SDKProvisioner (sdk.go)
// satisfies the same interface for real deploys.
package awsapi

import "context"

// DomainSpec describes the desired OpenSearch Service 3.1 domain (D14: the
// pinned version line applies here too — engine_version is never
// unpinned-dev in this package).
type DomainSpec struct {
	Name          string
	EngineVersion string // "OpenSearch_3.1" — pinned, D14
	InstanceType  string
	InstanceCount int
	VolumeSizeGB  int
}

// DomainState is what actually exists.
type DomainState struct {
	ARN      string
	Endpoint string
	Status   string // "creating" | "active"
	Spec     DomainSpec
}

// VPCSpec describes the desired network the ECS services and the OpenSearch
// domain run in.
type VPCSpec struct {
	Name string
	CIDR string
}

// VPCState is what actually exists.
type VPCState struct {
	ID   string
	CIDR string
}

// SecretSpec describes a secret Converge ensures exists. Value is the
// initial value at creation time only — Converge never diffs or logs it
// (rotation is a runbook procedure, not a Converge concern), so re-running
// Converge against an existing secret never re-reads or re-prints Value.
type SecretSpec struct {
	Name  string
	Value string
}

// SecretState is what actually exists — never carries the secret value.
type SecretState struct {
	ARN       string
	VersionID string
}

// ServiceSpec describes one ECS service's desired shape (D24's file scope:
// engramd, worker, and the co-located BGE-M3 embedding container are each
// one ServiceSpec in an Environment).
type ServiceSpec struct {
	Cluster       string
	Name          string
	Image         string
	CPU           int
	MemoryMB      int
	DesiredCount  int
	ContainerPort int
}

// ServiceState is what actually exists, including the blue/green history
// Rollback needs: ActiveTaskDefinitionARN is the revision currently running,
// PreviousTaskDefinitionARN is the one revision back Rollback reverts to
// (empty before any update has ever happened).
type ServiceState struct {
	ARN                       string
	ActiveTaskDefinitionARN   string
	PreviousTaskDefinitionARN string
	// Image / CPU / MemoryMB / ContainerPort are the observed task-definition
	// shape the drift check (serviceMatches) compares against the desired
	// ServiceSpec. All four live in the task definition, not the ECS service,
	// so a change to any of them at an unchanged desired count would
	// otherwise read as "unchanged" and silently no-op the rollout — the same
	// class of bug the Image field closed. Every task-def-carried field the
	// deploy declares is folded in here so no spec change converges silently.
	Image         string
	CPU           int
	MemoryMB      int
	ContainerPort int
	DesiredCount  int
	Status        string
}

// Provisioner is the Converge/Rollback AWS port. Every method is
// describe-then-act shaped: Describe* never mutates, Create*/Update*/
// Register* always do — Converge calls a mutating method only when a
// Describe disagrees with the desired spec, which is what makes a second
// Converge run against unchanged input a true no-op.
type Provisioner interface {
	DescribeDomain(ctx context.Context, name string) (DomainState, bool, error)
	CreateDomain(ctx context.Context, spec DomainSpec) (DomainState, error)

	DescribeVPC(ctx context.Context, name string) (VPCState, bool, error)
	CreateVPC(ctx context.Context, spec VPCSpec) (VPCState, error)

	DescribeSecret(ctx context.Context, name string) (SecretState, bool, error)
	CreateSecret(ctx context.Context, spec SecretSpec) (SecretState, error)

	DescribeService(ctx context.Context, cluster, name string) (ServiceState, bool, error)
	// RegisterTaskDefinition registers a new revision (never mutates an
	// existing one — ECS task definitions are immutable by design, which is
	// exactly what makes blue/green rollback possible: the prior revision
	// ARN always still resolves).
	RegisterTaskDefinition(ctx context.Context, spec ServiceSpec) (taskDefinitionARN string, err error)
	CreateService(ctx context.Context, spec ServiceSpec, taskDefinitionARN string) (ServiceState, error)
	// UpdateService points name at taskDefinitionARN (a blue/green cutover:
	// forward when Converge detects drift, backward when Rollback calls it
	// with the previous revision).
	UpdateService(ctx context.Context, cluster, name, taskDefinitionARN string, desiredCount int) (ServiceState, error)
}
