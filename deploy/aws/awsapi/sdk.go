package awsapi

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/opensearch"
	ostypes "github.com/aws/aws-sdk-go-v2/service/opensearch/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smithy "github.com/aws/smithy-go"
)

// SDKProvisioner is the real Provisioner: every method is a thin,
// describe-then-act translation onto aws-sdk-go-v2 clients (D24). It
// satisfies exactly the same interface as FakeProvisioner, so
// Converge/Rollback's logic (converge.go) is identical whether the target is
// the in-memory fake or a real AWS account — only the wiring in this file
// differs.
//
// This implementation is deliberately NOT exercised against real AWS in this
// build environment (no credentials available) — it compiles and is
// structurally complete for the four resource kinds Converge drives, but its
// correctness against a live account is a documented manual-verification
// step (see docs/runbooks and the phase report). Fargate networking
// (subnets/security groups) is intentionally out of VPCSpec's minimal shape;
// CreateService omits NetworkConfiguration, so a real Fargate deploy needs a
// follow-up operator step to attach the VPC's subnets — noted here rather
// than silently guessed at.
type SDKProvisioner struct {
	OpenSearch     *opensearch.Client
	ECS            *ecs.Client
	SecretsManager *secretsmanager.Client
	EC2            *ec2.Client
}

var _ Provisioner = (*SDKProvisioner)(nil)

// NewSDKProvisioner loads the default AWS config (environment/shared config
// per aws-sdk-go-v2's usual resolution order) and builds the four service
// clients Converge needs. It performs no network calls itself.
func NewSDKProvisioner(ctx context.Context, optFns ...func(*config.LoadOptions) error) (*SDKProvisioner, error) {
	cfg, err := config.LoadDefaultConfig(ctx, optFns...)
	if err != nil {
		return nil, fmt.Errorf("awsapi: loading AWS config: %w", err)
	}
	return &SDKProvisioner{
		OpenSearch:     opensearch.NewFromConfig(cfg),
		ECS:            ecs.NewFromConfig(cfg),
		SecretsManager: secretsmanager.NewFromConfig(cfg),
		EC2:            ec2.NewFromConfig(cfg),
	}, nil
}

// isNotFound reports whether err is the "resource does not exist" shape
// each AWS SDK service uses for its own resource kind — the describe-first
// idiom's "ok=false" case rather than a real failure.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "ResourceNotFoundException", "ResourceNotFoundFault", "InvalidParameterValue":
			return true
		}
	}
	return false
}

// DescribeDomain implements Provisioner.
func (p *SDKProvisioner) DescribeDomain(ctx context.Context, name string) (DomainState, bool, error) {
	out, err := p.OpenSearch.DescribeDomain(ctx, &opensearch.DescribeDomainInput{DomainName: aws.String(name)})
	if isNotFound(err) {
		return DomainState{}, false, nil
	}
	if err != nil {
		return DomainState{}, false, fmt.Errorf("awsapi: opensearch DescribeDomain %s: %w", name, err)
	}
	d := out.DomainStatus
	state := DomainState{ARN: aws.ToString(d.ARN), Status: "active"}
	if d.Endpoint != nil {
		state.Endpoint = *d.Endpoint
	}
	if d.Created != nil && !*d.Created {
		state.Status = "creating"
	}
	return state, true, nil
}

// CreateDomain implements Provisioner.
func (p *SDKProvisioner) CreateDomain(ctx context.Context, spec DomainSpec) (DomainState, error) {
	out, err := p.OpenSearch.CreateDomain(ctx, &opensearch.CreateDomainInput{
		DomainName:    aws.String(spec.Name),
		EngineVersion: aws.String(spec.EngineVersion), // pinned 3.1 line (D14)
		ClusterConfig: &ostypes.ClusterConfig{
			InstanceType:  ostypes.OpenSearchPartitionInstanceType(spec.InstanceType),
			InstanceCount: aws.Int32(int32(spec.InstanceCount)),
		},
		EBSOptions: &ostypes.EBSOptions{
			EBSEnabled: aws.Bool(true),
			VolumeSize: aws.Int32(int32(spec.VolumeSizeGB)),
			VolumeType: ostypes.VolumeTypeGp3,
		},
	})
	if err != nil {
		return DomainState{}, fmt.Errorf("awsapi: opensearch CreateDomain %s: %w", spec.Name, err)
	}
	return DomainState{ARN: aws.ToString(out.DomainStatus.ARN), Endpoint: aws.ToString(out.DomainStatus.Endpoint), Status: "creating", Spec: spec}, nil
}

// DescribeVPC implements Provisioner.
func (p *SDKProvisioner) DescribeVPC(ctx context.Context, name string) (VPCState, bool, error) {
	out, err := p.EC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []ec2types.Filter{{Name: aws.String("tag:Name"), Values: []string{name}}},
	})
	if err != nil {
		return VPCState{}, false, fmt.Errorf("awsapi: ec2 DescribeVpcs %s: %w", name, err)
	}
	if len(out.Vpcs) == 0 {
		return VPCState{}, false, nil
	}
	v := out.Vpcs[0]
	return VPCState{ID: aws.ToString(v.VpcId), CIDR: aws.ToString(v.CidrBlock)}, true, nil
}

// CreateVPC implements Provisioner.
func (p *SDKProvisioner) CreateVPC(ctx context.Context, spec VPCSpec) (VPCState, error) {
	out, err := p.EC2.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock: aws.String(spec.CIDR),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeVpc,
			Tags:         []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String(spec.Name)}},
		}},
	})
	if err != nil {
		return VPCState{}, fmt.Errorf("awsapi: ec2 CreateVpc %s: %w", spec.Name, err)
	}
	return VPCState{ID: aws.ToString(out.Vpc.VpcId), CIDR: aws.ToString(out.Vpc.CidrBlock)}, nil
}

// DescribeSecret implements Provisioner.
func (p *SDKProvisioner) DescribeSecret(ctx context.Context, name string) (SecretState, bool, error) {
	out, err := p.SecretsManager.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(name)})
	if isNotFound(err) {
		return SecretState{}, false, nil
	}
	if err != nil {
		return SecretState{}, false, fmt.Errorf("awsapi: secretsmanager DescribeSecret %s: %w", name, err)
	}
	// DescribeSecretOutput never carries the value (by design) — only ARN and
	// version metadata, which is all Converge needs to decide "exists".
	versionID := ""
	for id := range out.VersionIdsToStages {
		versionID = id
		break
	}
	return SecretState{ARN: aws.ToString(out.ARN), VersionID: versionID}, true, nil
}

// CreateSecret writes spec.Value exactly once (secret creation), never logs
// it, and never reads it back — the "no secret values in logs" constraint
// applies to this call site specifically since it is the one place a
// plaintext value is ever in memory here.
func (p *SDKProvisioner) CreateSecret(ctx context.Context, spec SecretSpec) (SecretState, error) {
	out, err := p.SecretsManager.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name:         aws.String(spec.Name),
		SecretString: aws.String(spec.Value),
	})
	if err != nil {
		return SecretState{}, fmt.Errorf("awsapi: secretsmanager CreateSecret %s: %w", spec.Name, err)
	}
	return SecretState{ARN: aws.ToString(out.ARN), VersionID: aws.ToString(out.VersionId)}, nil
}

// DescribeService implements Provisioner. It resolves the running task-def-
// carried shape (image + cpu/mem/port) from the service's active task
// definition (a second DescribeTaskDefinition call — these live in the task
// definition, not the service) so the drift check can detect a change to any
// of them.
func (p *SDKProvisioner) DescribeService(ctx context.Context, cluster, name string) (ServiceState, bool, error) {
	out, err := p.ECS.DescribeServices(ctx, &ecs.DescribeServicesInput{Cluster: aws.String(cluster), Services: []string{name}})
	if err != nil {
		return ServiceState{}, false, fmt.Errorf("awsapi: ecs DescribeServices %s/%s: %w", cluster, name, err)
	}
	if len(out.Services) == 0 {
		return ServiceState{}, false, nil
	}
	svc := out.Services[0]
	taskDefARN := aws.ToString(svc.TaskDefinition)
	shape, err := p.taskDefinitionShape(ctx, taskDefARN)
	if err != nil {
		return ServiceState{}, false, fmt.Errorf("awsapi: resolving task-def shape for %s/%s: %w", cluster, name, err)
	}
	// Normalize AWS's UPPER_CASE statuses onto the lowercase vocabulary
	// serviceMatches (converge.go) checks against; only ACTIVE maps to our
	// "active" — anything else (DRAINING, INACTIVE) is surfaced as-is so
	// Converge treats it as not-yet-matching and re-converges.
	status := aws.ToString(svc.Status)
	if status == "ACTIVE" || status == "" {
		status = "active"
	}
	return ServiceState{
		ARN: aws.ToString(svc.ServiceArn), ActiveTaskDefinitionARN: taskDefARN,
		Image: shape.image, CPU: shape.cpu, MemoryMB: shape.memoryMB, ContainerPort: shape.containerPort,
		DesiredCount: int(svc.DesiredCount), Status: status,
	}, true, nil
}

// taskDefinitionShape returns the first container's image + the task-def's
// cpu/memory + the first container port for a task-definition ARN. An empty
// ARN (a service with no task definition yet) yields a zero shape, not an
// error.
func (p *SDKProvisioner) taskDefinitionShape(ctx context.Context, taskDefARN string) (taskDefShape, error) {
	if taskDefARN == "" {
		return taskDefShape{}, nil
	}
	out, err := p.ECS.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String(taskDefARN)})
	if err != nil {
		return taskDefShape{}, fmt.Errorf("awsapi: ecs DescribeTaskDefinition %s: %w", taskDefARN, err)
	}
	td := out.TaskDefinition
	if td == nil || len(td.ContainerDefinitions) == 0 {
		return taskDefShape{}, nil
	}
	container := td.ContainerDefinitions[0]
	shape := taskDefShape{
		image:    aws.ToString(container.Image),
		cpu:      atoiOrZero(aws.ToString(td.Cpu)),
		memoryMB: atoiOrZero(aws.ToString(td.Memory)),
	}
	if len(container.PortMappings) > 0 && container.PortMappings[0].ContainerPort != nil {
		shape.containerPort = int(*container.PortMappings[0].ContainerPort)
	}
	return shape, nil
}

// atoiOrZero parses an ECS Cpu/Memory string (registered by
// RegisterTaskDefinition as a plain integer) back to an int, yielding 0 for
// an empty or unparseable value — a best-effort observed reading, never a
// hard failure.
func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// RegisterTaskDefinition implements Provisioner.
func (p *SDKProvisioner) RegisterTaskDefinition(ctx context.Context, spec ServiceSpec) (string, error) {
	container := ecstypes.ContainerDefinition{
		Name:  aws.String(spec.Name),
		Image: aws.String(spec.Image),
	}
	if spec.ContainerPort > 0 {
		container.PortMappings = []ecstypes.PortMapping{{ContainerPort: aws.Int32(int32(spec.ContainerPort))}}
	}
	out, err := p.ECS.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family:               aws.String(spec.Name),
		ContainerDefinitions: []ecstypes.ContainerDefinition{container},
		Cpu:                  aws.String(fmt.Sprintf("%d", spec.CPU)),
		Memory:               aws.String(fmt.Sprintf("%d", spec.MemoryMB)),
	})
	if err != nil {
		return "", fmt.Errorf("awsapi: ecs RegisterTaskDefinition %s: %w", spec.Name, err)
	}
	return aws.ToString(out.TaskDefinition.TaskDefinitionArn), nil
}

// CreateService implements Provisioner.
func (p *SDKProvisioner) CreateService(ctx context.Context, spec ServiceSpec, taskDefinitionARN string) (ServiceState, error) {
	out, err := p.ECS.CreateService(ctx, &ecs.CreateServiceInput{
		Cluster:        aws.String(spec.Cluster),
		ServiceName:    aws.String(spec.Name),
		TaskDefinition: aws.String(taskDefinitionARN),
		DesiredCount:   aws.Int32(int32(spec.DesiredCount)),
	})
	if err != nil {
		return ServiceState{}, fmt.Errorf("awsapi: ecs CreateService %s: %w", spec.Name, err)
	}
	return ServiceState{
		ARN: aws.ToString(out.Service.ServiceArn), ActiveTaskDefinitionARN: taskDefinitionARN,
		Image: spec.Image, CPU: spec.CPU, MemoryMB: spec.MemoryMB, ContainerPort: spec.ContainerPort,
		DesiredCount: spec.DesiredCount, Status: "active",
	}, nil
}

// UpdateService implements Provisioner.
func (p *SDKProvisioner) UpdateService(ctx context.Context, cluster, name, taskDefinitionARN string, desiredCount int) (ServiceState, error) {
	prev, ok, err := p.DescribeService(ctx, cluster, name)
	if err != nil {
		return ServiceState{}, err
	}
	if !ok {
		return ServiceState{}, fmt.Errorf("awsapi: ecs UpdateService %s/%s: service does not exist", cluster, name)
	}
	out, err := p.ECS.UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster:        aws.String(cluster),
		Service:        aws.String(name),
		TaskDefinition: aws.String(taskDefinitionARN),
		DesiredCount:   aws.Int32(int32(desiredCount)),
	})
	if err != nil {
		return ServiceState{}, fmt.Errorf("awsapi: ecs UpdateService %s/%s: %w", cluster, name, err)
	}
	shape, err := p.taskDefinitionShape(ctx, taskDefinitionARN)
	if err != nil {
		return ServiceState{}, fmt.Errorf("awsapi: resolving updated task-def shape for %s/%s: %w", cluster, name, err)
	}
	return ServiceState{
		ARN: aws.ToString(out.Service.ServiceArn), ActiveTaskDefinitionARN: taskDefinitionARN,
		PreviousTaskDefinitionARN: prev.ActiveTaskDefinitionARN,
		Image:                     shape.image, CPU: shape.cpu, MemoryMB: shape.memoryMB, ContainerPort: shape.containerPort,
		DesiredCount: desiredCount, Status: "active",
	}, nil
}
