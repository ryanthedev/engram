package awsapi_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ryanthedev/engram/deploy/aws/awsapi"
)

func testEnv() awsapi.Environment {
	return awsapi.Environment{
		Name: "staging",
		VPC:  awsapi.VPCSpec{Name: "engram-staging-vpc", CIDR: "10.10.0.0/16"},
		Domain: awsapi.DomainSpec{
			Name: "engram-staging", EngineVersion: "OpenSearch_3.1",
			InstanceType: "r6g.large.search", InstanceCount: 2, VolumeSizeGB: 100,
		},
		Secrets: []awsapi.SecretSpec{
			{Name: "engram-staging/extract-api-key", Value: "sk-fixture-not-real"},
		},
		Services: []awsapi.ServiceSpec{
			{Cluster: "engram-staging", Name: "engramd", Image: "engram:v1", CPU: 512, MemoryMB: 1024, DesiredCount: 2, ContainerPort: 7070},
			{Cluster: "engram-staging", Name: "worker", Image: "engram:v1", CPU: 512, MemoryMB: 1024, DesiredCount: 2, ContainerPort: 0},
			{Cluster: "engram-staging", Name: "embed", Image: "bge-m3:v1", CPU: 1024, MemoryMB: 2048, DesiredCount: 1, ContainerPort: 8081},
		},
	}
}

// TestConverge_CreatesMissingResources proves a Converge run against an
// empty Provisioner creates every declared resource exactly once.
func TestConverge_CreatesMissingResources(t *testing.T) {
	p := awsapi.NewFakeProvisioner()
	env := testEnv()

	rep, err := awsapi.Converge(context.Background(), p, env)
	if err != nil {
		t.Fatalf("Converge: %v", err)
	}
	if !rep.Changed() {
		t.Fatal("first Converge should report Changed()==true")
	}
	wantCreated := []string{
		"vpc:engram-staging-vpc", "domain:engram-staging",
		"secret:engram-staging/extract-api-key",
		"service:engramd", "service:worker", "service:embed",
	}
	for _, key := range wantCreated {
		if rep.Actions[key] != "created" {
			t.Errorf("action[%s] = %q, want created", key, rep.Actions[key])
		}
	}
}

// TestConverge_Idempotent_SecondRunNoOp is the DW-7.1 idempotency contract:
// re-running Converge against an unchanged Environment performs ZERO
// mutating Provisioner calls and reports every resource unchanged.
func TestConverge_Idempotent_SecondRunNoOp(t *testing.T) {
	p := awsapi.NewFakeProvisioner()
	env := testEnv()
	ctx := context.Background()

	if _, err := awsapi.Converge(ctx, p, env); err != nil {
		t.Fatalf("first Converge: %v", err)
	}
	callsAfterFirst := p.MutatingCalls.Load()
	if callsAfterFirst == 0 {
		t.Fatal("first Converge should have made mutating calls")
	}

	rep, err := awsapi.Converge(ctx, p, env)
	if err != nil {
		t.Fatalf("second Converge: %v", err)
	}
	if rep.Changed() {
		t.Fatalf("second Converge should be a no-op, got actions: %+v", rep.Actions)
	}
	if got := p.MutatingCalls.Load(); got != callsAfterFirst {
		t.Fatalf("second Converge made %d mutating calls (from %d), want 0 additional", got-callsAfterFirst, callsAfterFirst)
	}
	for key, action := range rep.Actions {
		if action != "unchanged" {
			t.Errorf("action[%s] = %q, want unchanged", key, action)
		}
	}
}

// TestConverge_DetectsDrift_UpdatesOnlyTheDriftedService proves Converge
// diffs each ECS service against its running state and only touches the one
// whose desired count actually changed — a blue/green revision bump, not a
// full re-create.
func TestConverge_DetectsDrift_UpdatesOnlyTheDriftedService(t *testing.T) {
	p := awsapi.NewFakeProvisioner()
	env := testEnv()
	ctx := context.Background()

	if _, err := awsapi.Converge(ctx, p, env); err != nil {
		t.Fatalf("first Converge: %v", err)
	}
	before, _, err := p.DescribeService(ctx, "engram-staging", "engramd")
	if err != nil {
		t.Fatalf("DescribeService: %v", err)
	}

	drifted := env
	drifted.Services = append([]awsapi.ServiceSpec{}, env.Services...)
	drifted.Services[0].DesiredCount = 5 // scale engramd up

	rep, err := awsapi.Converge(ctx, p, drifted)
	if err != nil {
		t.Fatalf("drifted Converge: %v", err)
	}
	if rep.Actions["service:engramd"] != "updated" {
		t.Fatalf("action[service:engramd] = %q, want updated", rep.Actions["service:engramd"])
	}
	if rep.Actions["service:worker"] != "unchanged" || rep.Actions["service:embed"] != "unchanged" {
		t.Fatalf("undrifted services should stay unchanged, got: %+v", rep.Actions)
	}

	after, _, err := p.DescribeService(ctx, "engram-staging", "engramd")
	if err != nil {
		t.Fatalf("DescribeService after update: %v", err)
	}
	if after.DesiredCount != 5 {
		t.Fatalf("engramd desired count = %d, want 5", after.DesiredCount)
	}
	if after.ActiveTaskDefinitionARN == before.ActiveTaskDefinitionARN {
		t.Fatal("blue/green update should register a NEW task-definition revision, not reuse the old ARN")
	}
	if after.PreviousTaskDefinitionARN != before.ActiveTaskDefinitionARN {
		t.Fatalf("PreviousTaskDefinitionARN = %q, want the pre-update active ARN %q", after.PreviousTaskDefinitionARN, before.ActiveTaskDefinitionARN)
	}
}

// TestConverge_ImageChange_RollsOut is the DW-7.1 core-purpose regression:
// a release deploys a NEW image tag at an UNCHANGED desired count (exactly
// what the CI deploy-staging/deploy-prod steps do, deploying by SHA tag).
// Converge must detect the image drift and roll it out — register a new
// task-definition revision and update the service — not silently no-op.
func TestConverge_ImageChange_RollsOut(t *testing.T) {
	p := awsapi.NewFakeProvisioner()
	env := testEnv() // all services at "engram:v1" / "bge-m3:v1"
	ctx := context.Background()

	if _, err := awsapi.Converge(ctx, p, env); err != nil {
		t.Fatalf("first Converge: %v", err)
	}
	before, _, err := p.DescribeService(ctx, "engram-staging", "engramd")
	if err != nil {
		t.Fatalf("DescribeService: %v", err)
	}
	if before.Image != "engram:v1" {
		t.Fatalf("engramd image after first converge = %q, want engram:v1", before.Image)
	}
	callsAfterFirst := p.MutatingCalls.Load()

	// A new release: same desired counts, new image tag on every service.
	release := env
	release.Services = append([]awsapi.ServiceSpec{}, env.Services...)
	for i := range release.Services {
		release.Services[i].Image = "engram:v2" // the new SHA-tagged image
	}

	rep, err := awsapi.Converge(ctx, p, release)
	if err != nil {
		t.Fatalf("release Converge: %v", err)
	}
	if !rep.Changed() {
		t.Fatal("an image-only release must NOT be a no-op — the new image would never roll out")
	}
	if p.MutatingCalls.Load() == callsAfterFirst {
		t.Fatal("image-only release performed zero mutating calls — the new image was never rolled out")
	}
	for _, svc := range []string{"engramd", "worker", "embed"} {
		if got := rep.Actions["service:"+svc]; got != "updated" {
			t.Errorf("action[service:%s] = %q, want updated (image changed)", svc, got)
		}
	}

	after, _, err := p.DescribeService(ctx, "engram-staging", "engramd")
	if err != nil {
		t.Fatalf("DescribeService after release: %v", err)
	}
	if after.Image != "engram:v2" {
		t.Fatalf("engramd image after release = %q, want engram:v2 (the new image is actually running)", after.Image)
	}
	if after.ActiveTaskDefinitionARN == before.ActiveTaskDefinitionARN {
		t.Fatal("an image change should register a NEW task-definition revision, not reuse the old ARN")
	}
	// DesiredCount is unchanged across the release — the drift that triggered
	// the rollout was the image alone, not a scaling change.
	if after.DesiredCount != before.DesiredCount {
		t.Fatalf("desired count changed unexpectedly: before=%d after=%d", before.DesiredCount, after.DesiredCount)
	}

	// Idempotency still holds at the new image: converging v2 again is a
	// verified no-op (no new mutating calls, everything unchanged).
	callsAfterRelease := p.MutatingCalls.Load()
	rep2, err := awsapi.Converge(ctx, p, release)
	if err != nil {
		t.Fatalf("second release Converge: %v", err)
	}
	if rep2.Changed() {
		t.Fatalf("re-converging the same image must be a no-op, got: %+v", rep2.Actions)
	}
	if p.MutatingCalls.Load() != callsAfterRelease {
		t.Fatalf("re-converging the same image made %d extra mutating calls, want 0", p.MutatingCalls.Load()-callsAfterRelease)
	}
}

// TestConverge_ResourceSizingChange_RollsOut proves the drift check covers
// the rest of the task-def-carried shape, not just Image: a CPU/memory
// change at an unchanged desired count and image must still roll out (a new
// task-def revision), and re-converging the same sizing is then a verified
// no-op. Guards against the same silent-no-op class as the Image bug for the
// remaining task-def fields.
func TestConverge_ResourceSizingChange_RollsOut(t *testing.T) {
	p := awsapi.NewFakeProvisioner()
	env := testEnv()
	ctx := context.Background()

	if _, err := awsapi.Converge(ctx, p, env); err != nil {
		t.Fatalf("first Converge: %v", err)
	}
	before, _, err := p.DescribeService(ctx, "engram-staging", "engramd")
	if err != nil {
		t.Fatalf("DescribeService: %v", err)
	}
	callsAfterFirst := p.MutatingCalls.Load()

	// Resize engramd's CPU + memory only — same image, same desired count.
	resized := env
	resized.Services = append([]awsapi.ServiceSpec{}, env.Services...)
	resized.Services[0].CPU = env.Services[0].CPU * 2
	resized.Services[0].MemoryMB = env.Services[0].MemoryMB * 2

	rep, err := awsapi.Converge(ctx, p, resized)
	if err != nil {
		t.Fatalf("resized Converge: %v", err)
	}
	if rep.Actions["service:engramd"] != "updated" {
		t.Fatalf("action[service:engramd] = %q, want updated (CPU/memory changed)", rep.Actions["service:engramd"])
	}
	if rep.Actions["service:worker"] != "unchanged" || rep.Actions["service:embed"] != "unchanged" {
		t.Fatalf("unresized services should stay unchanged, got: %+v", rep.Actions)
	}
	if p.MutatingCalls.Load() == callsAfterFirst {
		t.Fatal("a CPU/memory-only change performed zero mutating calls — the resize was never rolled out")
	}

	after, _, err := p.DescribeService(ctx, "engram-staging", "engramd")
	if err != nil {
		t.Fatalf("DescribeService after resize: %v", err)
	}
	if after.CPU != before.CPU*2 || after.MemoryMB != before.MemoryMB*2 {
		t.Fatalf("observed sizing after resize = (cpu %d, mem %d), want (%d, %d)", after.CPU, after.MemoryMB, before.CPU*2, before.MemoryMB*2)
	}
	if after.Image != before.Image {
		t.Fatalf("image changed unexpectedly during a sizing-only resize: %q -> %q", before.Image, after.Image)
	}
	if after.ActiveTaskDefinitionARN == before.ActiveTaskDefinitionARN {
		t.Fatal("a sizing change should register a NEW task-definition revision")
	}

	// Idempotent at the new sizing.
	callsAfterResize := p.MutatingCalls.Load()
	rep2, err := awsapi.Converge(ctx, p, resized)
	if err != nil {
		t.Fatalf("second resized Converge: %v", err)
	}
	if rep2.Changed() {
		t.Fatalf("re-converging the same sizing must be a no-op, got: %+v", rep2.Actions)
	}
	if p.MutatingCalls.Load() != callsAfterResize {
		t.Fatalf("re-converging the same sizing made %d extra mutating calls, want 0", p.MutatingCalls.Load()-callsAfterResize)
	}
}

// TestConverge_ContainerPortChange_RollsOut proves a container-port change
// (the last task-def-carried field) also triggers a rollout rather than a
// silent no-op.
func TestConverge_ContainerPortChange_RollsOut(t *testing.T) {
	p := awsapi.NewFakeProvisioner()
	env := testEnv()
	ctx := context.Background()

	if _, err := awsapi.Converge(ctx, p, env); err != nil {
		t.Fatalf("first Converge: %v", err)
	}
	callsAfterFirst := p.MutatingCalls.Load()

	reported := env
	reported.Services = append([]awsapi.ServiceSpec{}, env.Services...)
	reported.Services[0].ContainerPort = env.Services[0].ContainerPort + 1

	rep, err := awsapi.Converge(ctx, p, reported)
	if err != nil {
		t.Fatalf("port-change Converge: %v", err)
	}
	if rep.Actions["service:engramd"] != "updated" {
		t.Fatalf("action[service:engramd] = %q, want updated (container port changed)", rep.Actions["service:engramd"])
	}
	if p.MutatingCalls.Load() == callsAfterFirst {
		t.Fatal("a container-port change performed zero mutating calls — never rolled out")
	}

	after, _, err := p.DescribeService(ctx, "engram-staging", "engramd")
	if err != nil {
		t.Fatalf("DescribeService after port change: %v", err)
	}
	if after.ContainerPort != env.Services[0].ContainerPort+1 {
		t.Fatalf("observed container port = %d, want %d", after.ContainerPort, env.Services[0].ContainerPort+1)
	}
}

// TestRollback_RevertsTaskDefinition is the DW-7 rollback contract: Rollback
// points the service back at the task-definition revision it ran before its
// most recent Converge-driven update.
func TestRollback_RevertsTaskDefinition(t *testing.T) {
	p := awsapi.NewFakeProvisioner()
	env := testEnv()
	ctx := context.Background()

	if _, err := awsapi.Converge(ctx, p, env); err != nil {
		t.Fatalf("first Converge: %v", err)
	}
	v1, _, _ := p.DescribeService(ctx, "engram-staging", "engramd")

	drifted := env
	drifted.Services = append([]awsapi.ServiceSpec{}, env.Services...)
	drifted.Services[0].DesiredCount = 5
	if _, err := awsapi.Converge(ctx, p, drifted); err != nil {
		t.Fatalf("drifted Converge: %v", err)
	}
	v2, _, _ := p.DescribeService(ctx, "engram-staging", "engramd")
	if v2.ActiveTaskDefinitionARN == v1.ActiveTaskDefinitionARN {
		t.Fatal("setup invariant broken: v2 should be a new revision")
	}

	rolledBack, err := awsapi.Rollback(ctx, p, "engram-staging", "engramd")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rolledBack.ActiveTaskDefinitionARN != v1.ActiveTaskDefinitionARN {
		t.Fatalf("rolled-back ActiveTaskDefinitionARN = %q, want the pre-drift revision %q", rolledBack.ActiveTaskDefinitionARN, v1.ActiveTaskDefinitionARN)
	}
	// Desired count is preserved across the rollback (rollback reverts the
	// task definition, not an operator's separate scaling decision).
	if rolledBack.DesiredCount != 5 {
		t.Fatalf("rolled-back DesiredCount = %d, want 5 (unchanged by rollback)", rolledBack.DesiredCount)
	}

	current, _, _ := p.DescribeService(ctx, "engram-staging", "engramd")
	if current.ActiveTaskDefinitionARN != v1.ActiveTaskDefinitionARN {
		t.Fatalf("service state after rollback = %+v, want active ARN %q", current, v1.ActiveTaskDefinitionARN)
	}
}

// TestRollback_NoPriorRevisionFails proves Rollback fails loudly rather than
// silently no-op-ing when a service has never been updated (nothing to
// revert to).
func TestRollback_NoPriorRevisionFails(t *testing.T) {
	p := awsapi.NewFakeProvisioner()
	env := testEnv()
	ctx := context.Background()
	if _, err := awsapi.Converge(ctx, p, env); err != nil {
		t.Fatalf("Converge: %v", err)
	}

	if _, err := awsapi.Rollback(ctx, p, "engram-staging", "engramd"); err == nil {
		t.Fatal("expected an error rolling back a service with no prior revision")
	}
}

// TestRollback_UnknownServiceFails proves Rollback does not panic or
// silently succeed against a service that was never created.
func TestRollback_UnknownServiceFails(t *testing.T) {
	p := awsapi.NewFakeProvisioner()
	if _, err := awsapi.Rollback(context.Background(), p, "engram-staging", "ghost"); err == nil {
		t.Fatal("expected an error rolling back an unknown service")
	}
}

// TestConverge_PropagatesProvisionerErrors proves a Provisioner failure
// aborts Converge with a wrapped error rather than silently continuing.
func TestConverge_PropagatesProvisionerErrors(t *testing.T) {
	p := &erroringProvisioner{FakeProvisioner: awsapi.NewFakeProvisioner(), failOn: "DescribeVPC"}
	if _, err := awsapi.Converge(context.Background(), p, testEnv()); err == nil {
		t.Fatal("expected Converge to propagate a Describe error")
	}
}

// erroringProvisioner wraps FakeProvisioner and injects a failure on one
// named method, for testing Converge's error-propagation path.
type erroringProvisioner struct {
	*awsapi.FakeProvisioner
	failOn string
}

func (e *erroringProvisioner) DescribeVPC(ctx context.Context, name string) (awsapi.VPCState, bool, error) {
	if e.failOn == "DescribeVPC" {
		return awsapi.VPCState{}, false, errors.New("simulated AWS API failure")
	}
	return e.FakeProvisioner.DescribeVPC(ctx, name)
}
