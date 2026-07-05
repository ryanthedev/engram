package awsapi

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// FakeProvisioner is an in-memory Provisioner: deterministic, no network,
// no AWS credentials. It is the DW-7.1 idempotency/rollback evidence's
// fixture — Converge/Rollback are exercised against it directly, proving
// the logic in converge.go without ever touching real AWS.
type FakeProvisioner struct {
	mu sync.Mutex

	domains  map[string]DomainState
	vpcs     map[string]VPCState
	secrets  map[string]SecretState
	services map[string]ServiceState
	// taskDefs counts revisions registered per service family, for
	// generating distinct, monotonically increasing ARNs.
	taskDefs map[string]int
	// taskDefShapes maps a registered task-definition ARN to the task-def-
	// carried shape it holds (image + cpu/mem/port) — mirroring real ECS,
	// where these live in the task definition, not the service.
	// Create/UpdateService resolve the observed ServiceState fields through
	// this so the drift check sees a change to any of them.
	taskDefShapes map[string]taskDefShape

	// MutatingCalls counts every Create*/Update*/Register* call — the
	// idempotency test's evidence that a second Converge performs zero of
	// them.
	MutatingCalls atomic.Int64
}

// taskDefShape is the subset of a ServiceSpec that a task definition carries
// (everything the drift check compares except identity and desired count).
type taskDefShape struct {
	image         string
	cpu           int
	memoryMB      int
	containerPort int
}

// NewFakeProvisioner returns an empty FakeProvisioner.
func NewFakeProvisioner() *FakeProvisioner {
	return &FakeProvisioner{
		domains:       map[string]DomainState{},
		vpcs:          map[string]VPCState{},
		secrets:       map[string]SecretState{},
		services:      map[string]ServiceState{},
		taskDefs:      map[string]int{},
		taskDefShapes: map[string]taskDefShape{},
	}
}

var _ Provisioner = (*FakeProvisioner)(nil)

// DescribeDomain implements Provisioner.
func (f *FakeProvisioner) DescribeDomain(_ context.Context, name string) (DomainState, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.domains[name]
	return d, ok, nil
}

// CreateDomain implements Provisioner.
func (f *FakeProvisioner) CreateDomain(_ context.Context, spec DomainSpec) (DomainState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.MutatingCalls.Add(1)
	d := DomainState{ARN: "arn:aws:es:fake:domain/" + spec.Name, Endpoint: spec.Name + ".fake.es.amazonaws.com", Status: "active", Spec: spec}
	f.domains[spec.Name] = d
	return d, nil
}

// DescribeVPC implements Provisioner.
func (f *FakeProvisioner) DescribeVPC(_ context.Context, name string) (VPCState, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vpcs[name]
	return v, ok, nil
}

// CreateVPC implements Provisioner.
func (f *FakeProvisioner) CreateVPC(_ context.Context, spec VPCSpec) (VPCState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.MutatingCalls.Add(1)
	v := VPCState{ID: "vpc-fake-" + spec.Name, CIDR: spec.CIDR}
	f.vpcs[spec.Name] = v
	return v, nil
}

// DescribeSecret implements Provisioner.
func (f *FakeProvisioner) DescribeSecret(_ context.Context, name string) (SecretState, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.secrets[name]
	return s, ok, nil
}

// CreateSecret stores the spec's presence only — never the value itself, in
// keeping with the "no secret values in the repo or logs" constraint even
// for this in-memory fake (defense in depth: a fake that never holds
// plaintext can never leak it into a test failure message either).
func (f *FakeProvisioner) CreateSecret(_ context.Context, spec SecretSpec) (SecretState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.MutatingCalls.Add(1)
	s := SecretState{ARN: "arn:aws:secretsmanager:fake:secret:" + spec.Name, VersionID: "v1"}
	f.secrets[spec.Name] = s
	return s, nil
}

// DescribeService implements Provisioner.
func (f *FakeProvisioner) DescribeService(_ context.Context, _, name string) (ServiceState, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.services[name]
	return s, ok, nil
}

// RegisterTaskDefinition implements Provisioner: each call mints a new,
// distinct revision ARN (never reuses one) — the invariant Rollback's
// "revert to the previous revision" logic depends on.
func (f *FakeProvisioner) RegisterTaskDefinition(_ context.Context, spec ServiceSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.MutatingCalls.Add(1)
	f.taskDefs[spec.Name]++
	arn := fmt.Sprintf("arn:aws:ecs:fake:task-definition/%s:%d", spec.Name, f.taskDefs[spec.Name])
	// The image/cpu/mem/port all live in the task definition.
	f.taskDefShapes[arn] = taskDefShape{image: spec.Image, cpu: spec.CPU, memoryMB: spec.MemoryMB, containerPort: spec.ContainerPort}
	return arn, nil
}

// serviceStateFromTaskDef builds the task-def-carried fields of a
// ServiceState from a registered ARN's stored shape.
func (f *FakeProvisioner) shapeOf(arn string) taskDefShape { return f.taskDefShapes[arn] }

// CreateService implements Provisioner.
func (f *FakeProvisioner) CreateService(_ context.Context, spec ServiceSpec, taskDefARN string) (ServiceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.MutatingCalls.Add(1)
	shape := f.shapeOf(taskDefARN)
	s := ServiceState{
		ARN:                     "arn:aws:ecs:fake:service/" + spec.Name,
		ActiveTaskDefinitionARN: taskDefARN,
		Image:                   shape.image,
		CPU:                     shape.cpu,
		MemoryMB:                shape.memoryMB,
		ContainerPort:           shape.containerPort,
		DesiredCount:            spec.DesiredCount,
		Status:                  "active",
	}
	f.services[spec.Name] = s
	return s, nil
}

// UpdateService implements Provisioner: it shifts the current
// ActiveTaskDefinitionARN down to PreviousTaskDefinitionARN before applying
// the new one — the blue/green history Rollback reads.
func (f *FakeProvisioner) UpdateService(_ context.Context, _, name, taskDefARN string, desiredCount int) (ServiceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.MutatingCalls.Add(1)
	prev, ok := f.services[name]
	if !ok {
		return ServiceState{}, fmt.Errorf("awsapi: fake: UpdateService on unknown service %s", name)
	}
	shape := f.shapeOf(taskDefARN)
	next := ServiceState{
		ARN:                       prev.ARN,
		ActiveTaskDefinitionARN:   taskDefARN,
		PreviousTaskDefinitionARN: prev.ActiveTaskDefinitionARN,
		Image:                     shape.image,
		CPU:                       shape.cpu,
		MemoryMB:                  shape.memoryMB,
		ContainerPort:             shape.containerPort,
		DesiredCount:              desiredCount,
		Status:                    "active",
	}
	f.services[name] = next
	return next, nil
}
