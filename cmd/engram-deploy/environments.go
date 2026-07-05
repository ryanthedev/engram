package main

import (
	"fmt"

	"github.com/ryanthedev/engram/deploy/aws/awsapi"
)

// stagingEnvironment and prodEnvironment are topology-identical (D22): the
// same three ECS services (engramd, worker, the co-located BGE-M3 embedding
// container) and the same OpenSearch Service 3.1 domain shape, scaled down
// for staging. image is the container image tag being deployed — the one
// value that legitimately differs between a routine Converge run and a
// release promotion.
func stagingEnvironment(image string) awsapi.Environment {
	return environment("staging", image, 1, 1)
}

func prodEnvironment(image string) awsapi.Environment {
	return environment("prod", image, 2, 3)
}

// environment builds one topology-identical Environment. osInstances is the
// OpenSearch data-node count; desiredCount is the ECS desired count applied
// to engramd/worker uniformly (the embedding container always runs at 1 —
// co-located BGE-M3 serving is not horizontally scaled at S1, D15).
func environment(name, image string, osInstances, desiredCount int) awsapi.Environment {
	return awsapi.Environment{
		Name: name,
		VPC:  awsapi.VPCSpec{Name: fmt.Sprintf("engram-%s-vpc", name), CIDR: "10.20.0.0/16"},
		Domain: awsapi.DomainSpec{
			Name:          fmt.Sprintf("engram-%s", name),
			EngineVersion: "OpenSearch_3.1", // pinned — D14
			InstanceType:  "r6g.large.search",
			InstanceCount: osInstances,
			VolumeSizeGB:  100,
		},
		Secrets: []awsapi.SecretSpec{
			// Value is a placeholder here deliberately: Converge only
			// CREATES a missing secret, never diffs or re-logs its value —
			// the real value is provisioned out of band (docs/runbooks/
			// secrets-rotation.md), never checked into this repo.
			{Name: fmt.Sprintf("engram-%s/extract-api-key", name), Value: "REPLACE_VIA_ROTATION_RUNBOOK"},
			{Name: fmt.Sprintf("engram-%s/gate-api-key", name), Value: "REPLACE_VIA_ROTATION_RUNBOOK"},
		},
		// All three services run the same built image (deploy/local/Dockerfile's
		// pattern: one image, per-service ECS container command selects the
		// binary — engram-server, or a future standalone worker/embed-server
		// entrypoint) — mirroring the compose stack rather than inventing a
		// second image per service.
		Services: []awsapi.ServiceSpec{
			{Cluster: "engram-" + name, Name: "engramd", Image: image, CPU: 1024, MemoryMB: 2048, DesiredCount: desiredCount, ContainerPort: 7070},
			{Cluster: "engram-" + name, Name: "worker", Image: image, CPU: 1024, MemoryMB: 2048, DesiredCount: desiredCount, ContainerPort: 0},
			{Cluster: "engram-" + name, Name: "embed", Image: image, CPU: 2048, MemoryMB: 4096, DesiredCount: 1, ContainerPort: 8081},
		},
	}
}
