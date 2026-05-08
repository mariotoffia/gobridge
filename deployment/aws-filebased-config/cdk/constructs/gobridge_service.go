package constructs

import (
	"encoding/json"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsapplicationautoscaling"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// GoBridgeServiceProps configures the Fargate service for gobridge.
type GoBridgeServiceProps struct {
	// Vpc is the VPC in which the Fargate service runs.
	Vpc awsec2.IVpc

	// Cluster is an existing ECS cluster. If nil, one is created.
	Cluster awsecs.ICluster

	// ServiceName is the ECS service name.
	ServiceName string

	// Image is the container image (e.g. ECR URI with tag).
	Image awsecs.ContainerImage

	// Bootstrap is the gobridge bootstrap configuration. The construct
	// serializes it as GOBRIDGE_FILEBASED_BOOTSTRAP_JSON env var.
	Bootstrap infra.BootstrapConfig

	// CPU is the Fargate task CPU units. Default: 512.
	CPU *float64

	// MemoryMiB is the Fargate task memory in MiB. Default: 1024.
	MemoryMiB *float64

	// DesiredCount is the number of tasks. Default: 1.
	DesiredCount *float64

	// EfsConfig provides the EFS filesystem and access point for config.
	// If nil, a default GoBridgeEfsConfig is created.
	EfsConfig *GoBridgeEfsConfig

	// ConfigMountPath is the container path where EFS is mounted.
	// Default: /mnt/gobridge
	ConfigMountPath *string

	// SsmParameterArns are additional SSM parameter ARNs for which the
	// task role needs ssm:GetParameter. The admin key param is added
	// automatically from Bootstrap.AdminAPIKeyParam.
	SsmParameterArns []*string

	// Exposure controls which ports get ALB target groups.
	Exposure infra.Exposure

	// LogRetention is the CloudWatch log retention. Default: ONE_WEEK.
	LogRetention awslogs.RetentionDays

	// ScalingMaxCapacity is the max task count for auto-scaling.
	// Default: 4. Set to 0 to disable scaling.
	ScalingMaxCapacity *float64

	// CpuTargetPercent is the CPU target for auto-scaling. Default: 70.
	CpuTargetPercent *float64
}

// GoBridgeService is the primary L2 construct for deploying gobridge
// on ECS Fargate with EFS config mounting and SSM secret access.
type GoBridgeService struct {
	constructs.Construct

	service awsecs.FargateService
	taskDef awsecs.FargateTaskDefinition
}

// NewGoBridgeService creates a new gobridge Fargate service.
func NewGoBridgeService(scope constructs.Construct, id *string, props *GoBridgeServiceProps) *GoBridgeService {
	c := constructs.NewConstruct(scope, id)

	cpu := jsii.Number(512)
	if props.CPU != nil {
		cpu = props.CPU
	}
	mem := jsii.Number(1024)
	if props.MemoryMiB != nil {
		mem = props.MemoryMiB
	}
	desired := jsii.Number(1)
	if props.DesiredCount != nil {
		desired = props.DesiredCount
	}
	mountPath := jsii.String("/mnt/gobridge")
	if props.ConfigMountPath != nil {
		mountPath = props.ConfigMountPath
	}
	logRetention := awslogs.RetentionDays_ONE_WEEK
	if props.LogRetention != "" {
		logRetention = props.LogRetention
	}

	// EFS
	efsConfig := props.EfsConfig
	if efsConfig == nil {
		efsConfig = NewGoBridgeEfsConfig(c, jsii.String("Efs"), &GoBridgeEfsConfigProps{
			Vpc: props.Vpc,
		})
	}

	// Cluster
	cluster := props.Cluster
	if cluster == nil {
		cluster = awsecs.NewCluster(c, jsii.String("Cluster"), &awsecs.ClusterProps{
			Vpc: props.Vpc,
		})
	}

	// Task definition
	taskDef := awsecs.NewFargateTaskDefinition(c, jsii.String("TaskDef"), &awsecs.FargateTaskDefinitionProps{
		Cpu:            cpu,
		MemoryLimitMiB: mem,
	})

	// EFS volume
	taskDef.AddVolume(&awsecs.Volume{
		Name: jsii.String("gobridge-config"),
		EfsVolumeConfiguration: &awsecs.EfsVolumeConfiguration{
			FileSystemId:      efsConfig.FileSystem().FileSystemId(),
			TransitEncryption: jsii.String("ENABLED"),
			AuthorizationConfig: &awsecs.AuthorizationConfig{
				AccessPointId: efsConfig.ControlAccessPoint().AccessPointId(),
				Iam:           jsii.String("ENABLED"),
			},
		},
	})

	// Serialize bootstrap config
	bootstrapJSON := marshalBootstrapJSON(props.Bootstrap)

	// Log group
	logGroup := awslogs.NewLogGroup(c, jsii.String("Logs"), &awslogs.LogGroupProps{
		Retention: logRetention,
	})

	// Container
	container := taskDef.AddContainer(jsii.String("gobridge"), &awsecs.ContainerDefinitionOptions{
		Image: props.Image,
		Logging: awsecs.LogDriver_AwsLogs(&awsecs.AwsLogDriverProps{
			LogGroup:     logGroup,
			StreamPrefix: jsii.String("gobridge"),
		}),
		Environment: &map[string]*string{
			"GOBRIDGE_FILEBASED_BOOTSTRAP_JSON": jsii.String(bootstrapJSON),
		},
		HealthCheck: &awsecs.HealthCheck{
			Command: &[]*string{
				jsii.String("CMD-SHELL"),
				jsii.String("wget -q --spider http://localhost:8080/healthz || exit 1"),
			},
			Interval: awscdk.Duration_Seconds(jsii.Number(30)),
			Timeout:  awscdk.Duration_Seconds(jsii.Number(5)),
			Retries:  jsii.Number(3),
		},
	})

	container.AddMountPoints(&awsecs.MountPoint{
		SourceVolume:  jsii.String("gobridge-config"),
		ContainerPath: mountPath,
		ReadOnly:      jsii.Bool(true),
	})

	addPortMappings(container, props.Exposure)

	// IAM: EFS access
	efsConfig.FileSystem().Grant(taskDef.TaskRole(),
		jsii.String("elasticfilesystem:ClientMount"),
		jsii.String("elasticfilesystem:ClientRead"),
	)

	// IAM: SSM parameter access
	grantSSMAccess(c, taskDef.TaskRole(), props)

	// Security group
	svcSG := awsec2.NewSecurityGroup(c, jsii.String("SvcSG"), &awsec2.SecurityGroupProps{
		Vpc:         props.Vpc,
		Description: jsii.String("gobridge Fargate task"),
	})

	// Allow task -> EFS (skipped when reusing an external filesystem
	// whose security group the operator owns).
	if sg := efsConfig.SecurityGroup(); sg != nil {
		sg.AddIngressRule(
			svcSG,
			awsec2.Port_Tcp(jsii.Number(2049)),
			jsii.String("gobridge task NFS access"),
			jsii.Bool(false),
		)
	}

	// Service
	svc := awsecs.NewFargateService(c, jsii.String("Svc"), &awsecs.FargateServiceProps{
		Cluster:        cluster,
		TaskDefinition: taskDef,
		DesiredCount:   desired,
		SecurityGroups: &[]awsec2.ISecurityGroup{svcSG},
		ServiceName:    jsii.String(props.ServiceName),
	})

	// Auto-scaling
	maxCap := 4.0
	if props.ScalingMaxCapacity != nil {
		maxCap = *props.ScalingMaxCapacity
	}
	if maxCap > 0 {
		cpuTarget := 70.0
		if props.CpuTargetPercent != nil {
			cpuTarget = *props.CpuTargetPercent
		}
		scaling := svc.AutoScaleTaskCount(&awsapplicationautoscaling.EnableScalingProps{
			MinCapacity: desired,
			MaxCapacity: jsii.Number(maxCap),
		})
		scaling.ScaleOnCpuUtilization(jsii.String("CpuScaling"), &awsecs.CpuUtilizationScalingProps{
			TargetUtilizationPercent: jsii.Number(cpuTarget),
		})
	}

	return &GoBridgeService{
		Construct: c,
		service:   svc,
		taskDef:   taskDef,
	}
}

// Service returns the underlying ECS Fargate service.
func (s *GoBridgeService) Service() awsecs.FargateService { return s.service }

// TaskDefinition returns the Fargate task definition.
func (s *GoBridgeService) TaskDefinition() awsecs.FargateTaskDefinition { return s.taskDef }

func marshalBootstrapJSON(cfg infra.BootstrapConfig) string {
	data, err := json.Marshal(cfg)
	if err != nil {
		panic("gobridge: failed to marshal bootstrap config: " + err.Error())
	}
	return string(data)
}

func addPortMappings(container awsecs.ContainerDefinition, exposure infra.Exposure) {
	// Always map the admin port for health checks.
	container.AddPortMappings(&awsecs.PortMapping{
		ContainerPort: jsii.Number(8080),
		Protocol:      awsecs.Protocol_TCP,
	})

	if exposure.Monitor {
		container.AddPortMappings(&awsecs.PortMapping{
			ContainerPort: jsii.Number(8081),
			Protocol:      awsecs.Protocol_TCP,
		})
	}
	if exposure.TransportHTTP {
		container.AddPortMappings(&awsecs.PortMapping{
			ContainerPort: jsii.Number(8082),
			Protocol:      awsecs.Protocol_TCP,
		})
	}
}

func grantSSMAccess(_ constructs.Construct, role awsiam.IRole, props *GoBridgeServiceProps) {
	if len(props.SsmParameterArns) == 0 {
		return
	}

	paramARNs := make([]*string, 0, len(props.SsmParameterArns))
	paramARNs = append(paramARNs, props.SsmParameterArns...)

	role.AddToPrincipalPolicy(awsiam.NewPolicyStatement(&awsiam.PolicyStatementProps{
		Actions:   &[]*string{jsii.String("ssm:GetParameter")},
		Resources: &paramARNs,
		Effect:    awsiam.Effect_ALLOW,
	}))
}
