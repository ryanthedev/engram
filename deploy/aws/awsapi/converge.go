package awsapi

import (
	"context"
	"fmt"
)

// Environment is one deploy target's full desired shape — staging and prod
// are each one Environment, topology-identical (D22) but scaled differently
// (instance counts, desired counts). Services holds one ServiceSpec per ECS
// service (engramd, worker, the co-located BGE-M3 embedding container).
type Environment struct {
	Name     string // "staging" | "prod"
	VPC      VPCSpec
	Domain   DomainSpec
	Secrets  []SecretSpec
	Services []ServiceSpec
}

// ConvergeReport records what one Converge run did per resource: "created"
// for a resource that did not exist, "updated" for a service whose spec
// drifted from what's running (a blue/green task-definition revision bump),
// "unchanged" for anything Describe already matched.
type ConvergeReport struct {
	Actions map[string]string
}

// Changed reports whether the run did anything mutating — false means a
// pure no-op re-application, the DW-7.1 idempotency contract.
func (r ConvergeReport) Changed() bool {
	for _, a := range r.Actions {
		if a != "unchanged" {
			return true
		}
	}
	return false
}

// Converge idempotently drives env's desired state against p: each resource
// is described first, created only if missing, and an ECS service is
// updated (a new task-definition revision + blue/green cutover) only if its
// running spec has actually drifted from env. Re-running Converge against an
// unchanged Environment performs zero mutating Provisioner calls — the
// DW-7.1 idempotency contract this function's unit tests (against
// FakeProvisioner) prove directly.
func Converge(ctx context.Context, p Provisioner, env Environment) (ConvergeReport, error) {
	rep := ConvergeReport{Actions: map[string]string{}}

	if _, ok, err := p.DescribeVPC(ctx, env.VPC.Name); err != nil {
		return rep, fmt.Errorf("awsapi: describing vpc %s: %w", env.VPC.Name, err)
	} else if !ok {
		if _, err := p.CreateVPC(ctx, env.VPC); err != nil {
			return rep, fmt.Errorf("awsapi: creating vpc %s: %w", env.VPC.Name, err)
		}
		rep.Actions["vpc:"+env.VPC.Name] = "created"
	} else {
		rep.Actions["vpc:"+env.VPC.Name] = "unchanged"
	}

	if _, ok, err := p.DescribeDomain(ctx, env.Domain.Name); err != nil {
		return rep, fmt.Errorf("awsapi: describing domain %s: %w", env.Domain.Name, err)
	} else if !ok {
		if _, err := p.CreateDomain(ctx, env.Domain); err != nil {
			return rep, fmt.Errorf("awsapi: creating domain %s: %w", env.Domain.Name, err)
		}
		rep.Actions["domain:"+env.Domain.Name] = "created"
	} else {
		// Converge never mutates an existing domain's spec (an OpenSearch
		// domain update is itself a blue/green operation with its own
		// caveats — DW-7 edge cases — out of scope for this converge pass;
		// domain resizing is a deliberate, reviewed operator action, not
		// something a routine Converge silently drifts toward).
		rep.Actions["domain:"+env.Domain.Name] = "unchanged"
	}

	for _, secret := range env.Secrets {
		if _, ok, err := p.DescribeSecret(ctx, secret.Name); err != nil {
			return rep, fmt.Errorf("awsapi: describing secret %s: %w", secret.Name, err)
		} else if !ok {
			if _, err := p.CreateSecret(ctx, secret); err != nil {
				return rep, fmt.Errorf("awsapi: creating secret %s: %w", secret.Name, err)
			}
			rep.Actions["secret:"+secret.Name] = "created"
		} else {
			// Never re-read or re-log the value on an existing secret —
			// rotation is a deliberate runbook action (docs/runbooks), not a
			// Converge side effect.
			rep.Actions["secret:"+secret.Name] = "unchanged"
		}
	}

	for _, svc := range env.Services {
		action, err := convergeService(ctx, p, svc)
		if err != nil {
			return rep, err
		}
		rep.Actions["service:"+svc.Name] = action
	}

	return rep, nil
}

// convergeService describes one ECS service and creates or blue/green-
// updates it to match spec, reporting which it did.
func convergeService(ctx context.Context, p Provisioner, spec ServiceSpec) (string, error) {
	state, ok, err := p.DescribeService(ctx, spec.Cluster, spec.Name)
	if err != nil {
		return "", fmt.Errorf("awsapi: describing service %s: %w", spec.Name, err)
	}
	if !ok {
		taskDefARN, err := p.RegisterTaskDefinition(ctx, spec)
		if err != nil {
			return "", fmt.Errorf("awsapi: registering task definition for %s: %w", spec.Name, err)
		}
		if _, err := p.CreateService(ctx, spec, taskDefARN); err != nil {
			return "", fmt.Errorf("awsapi: creating service %s: %w", spec.Name, err)
		}
		return "created", nil
	}
	if !serviceMatches(state, spec) {
		taskDefARN, err := p.RegisterTaskDefinition(ctx, spec)
		if err != nil {
			return "", fmt.Errorf("awsapi: registering task definition for %s: %w", spec.Name, err)
		}
		if _, err := p.UpdateService(ctx, spec.Cluster, spec.Name, taskDefARN, spec.DesiredCount); err != nil {
			return "", fmt.Errorf("awsapi: updating service %s: %w", spec.Name, err)
		}
		return "updated", nil
	}
	return "unchanged", nil
}

// serviceMatches reports whether the running service's observable shape
// already satisfies spec — the drift check that decides whether Converge
// needs a blue/green update at all. Every field the deploy declares in a
// ServiceSpec's task definition is compared: a change to the image (every
// SHA-tagged release), the CPU/memory sizing, or the container port at an
// unchanged desired count must trigger a new task-def revision + rollout,
// not read as "unchanged" and silently no-op.
//
// Cluster and Name are identity, not drift (they select WHICH service, they
// are not a change TO one), so they are deliberately excluded here.
func serviceMatches(state ServiceState, spec ServiceSpec) bool {
	return state.DesiredCount == spec.DesiredCount &&
		state.Image == spec.Image &&
		state.CPU == spec.CPU &&
		state.MemoryMB == spec.MemoryMB &&
		state.ContainerPort == spec.ContainerPort &&
		state.Status == "active"
}

// Rollback reverts one ECS service to the task-definition revision it ran
// before its most recent Converge-driven update — the blue/green revert the
// Phase-7 rollback contract requires. It fails loudly if there is no prior
// revision to revert to (a service that has never been updated), rather
// than silently no-op-ing on a rollback request that cannot be honored.
func Rollback(ctx context.Context, p Provisioner, cluster, service string) (ServiceState, error) {
	state, ok, err := p.DescribeService(ctx, cluster, service)
	if err != nil {
		return ServiceState{}, fmt.Errorf("awsapi: describing service %s for rollback: %w", service, err)
	}
	if !ok {
		return ServiceState{}, fmt.Errorf("awsapi: cannot roll back %s: service does not exist", service)
	}
	if state.PreviousTaskDefinitionARN == "" {
		return ServiceState{}, fmt.Errorf("awsapi: cannot roll back %s: no prior task-definition revision recorded", service)
	}
	return p.UpdateService(ctx, cluster, service, state.PreviousTaskDefinitionARN, state.DesiredCount)
}
