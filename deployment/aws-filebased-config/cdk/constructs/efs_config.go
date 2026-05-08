// Package constructs provides reusable CDK L2 constructs for deploying
// the gobridge file-based configuration profile on AWS ECS Fargate.
package constructs

import (
	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsbackup"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsefs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awskms"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
)

// GoBridgeEfsConfigProps configures the EFS filesystem and the two access
// points (Control RW, Worker RO) that back the gobridge cluster.
//
// Encryption is always on. Performance mode is locked to General Purpose.
// Both access points share root path "/" and identical POSIX identity;
// RW vs RO separation is enforced at IAM and ECS volume level (see T08
// grants and GoBridgeService mount config), NOT at POSIX user level.
type GoBridgeEfsConfigProps struct {
	// Vpc is the VPC in which EFS mount targets are created. Required.
	Vpc awsec2.IVpc

	// VpcSubnets selects the subnets for mount targets. If nil the
	// default is all private subnets in the VPC. The same selection
	// must be used by the ECS services consuming this filesystem; the
	// parent construct (GoBridgeSingle/Cluster) enforces that match.
	VpcSubnets *awsec2.SubnetSelection

	// FileSystem is an existing EFS filesystem to reuse. If nil a new
	// filesystem is created.
	FileSystem awsefs.IFileSystem

	// EfsKmsKey is an optional customer-managed KMS key used to encrypt
	// the filesystem at rest. If nil the AWS-managed EFS key is used.
	EfsKmsKey awskms.IKey

	// ThroughputMode overrides the default ELASTIC throughput mode.
	ThroughputMode awsefs.ThroughputMode

	// RemovalPolicy controls what happens to the filesystem on stack
	// deletion. Default: RETAIN.
	RemovalPolicy awscdk.RemovalPolicy

	// DisableBackup opts out of the default AWS Backup plan
	// (daily, 35-day retention). Default: false (backup enabled).
	DisableBackup bool

	// PosixUID is the POSIX user ID for both access points. Default: "1000".
	PosixUID *string

	// PosixGID is the POSIX group ID for both access points. Default: "1000".
	PosixGID *string
}

// GoBridgeEfsConfig is an L2 construct that creates (or reuses) an EFS
// filesystem with two access points - Control (intended RW) and Worker
// (intended RO) - sharing root path "/".
type GoBridgeEfsConfig struct {
	constructs.Construct

	fileSystem  awsefs.IFileSystem
	controlAP   awsefs.AccessPoint
	workerAP    awsefs.AccessPoint
	securityGrp awsec2.SecurityGroup
	vpcSubnets  *awsec2.SubnetSelection
}

// NewGoBridgeEfsConfig creates the EFS configuration construct.
func NewGoBridgeEfsConfig(scope constructs.Construct, id *string, props *GoBridgeEfsConfigProps) *GoBridgeEfsConfig {
	if props == nil {
		panic("GoBridgeEfsConfig: props must not be nil")
	}
	if props.Vpc == nil {
		panic("GoBridgeEfsConfig: Vpc is required")
	}

	c := constructs.NewConstruct(scope, id)

	posixUID := jsii.String("1000")
	if props.PosixUID != nil {
		posixUID = props.PosixUID
	}
	posixGID := jsii.String("1000")
	if props.PosixGID != nil {
		posixGID = props.PosixGID
	}

	subnetSelection := props.VpcSubnets
	if subnetSelection == nil {
		subnetSelection = &awsec2.SubnetSelection{
			SubnetType: awsec2.SubnetType_PRIVATE_WITH_EGRESS,
		}
	}
	selected := props.Vpc.SelectSubnets(subnetSelection)
	if selected == nil || selected.SubnetIds == nil || len(*selected.SubnetIds) == 0 {
		panic("GoBridgeEfsConfig: VpcSubnets selection resolved to zero subnets")
	}

	sg := awsec2.NewSecurityGroup(c, jsii.String("EfsSG"), &awsec2.SecurityGroupProps{
		Vpc:              props.Vpc,
		Description:      jsii.String("gobridge EFS mount target access"),
		AllowAllOutbound: jsii.Bool(false),
	})

	var fs awsefs.IFileSystem
	if props.FileSystem != nil {
		fs = props.FileSystem
	} else {
		throughput := awsefs.ThroughputMode_ELASTIC
		if props.ThroughputMode != "" {
			throughput = props.ThroughputMode
		}
		removal := awscdk.RemovalPolicy_RETAIN
		if props.RemovalPolicy != "" {
			removal = props.RemovalPolicy
		}

		fsProps := &awsefs.FileSystemProps{
			Vpc:             props.Vpc,
			VpcSubnets:      subnetSelection,
			Encrypted:       jsii.Bool(true),
			KmsKey:          props.EfsKmsKey,
			SecurityGroup:   sg,
			PerformanceMode: awsefs.PerformanceMode_GENERAL_PURPOSE,
			ThroughputMode:  throughput,
			RemovalPolicy:   removal,
		}
		fs = awsefs.NewFileSystem(c, jsii.String("Fs"), fsProps)
	}

	posixUser := &awsefs.PosixUser{Uid: posixUID, Gid: posixGID}
	createAcl := &awsefs.Acl{
		OwnerUid:    posixUID,
		OwnerGid:    posixGID,
		Permissions: jsii.String("755"),
	}

	controlAP := awsefs.NewAccessPoint(c, jsii.String("Control"), &awsefs.AccessPointProps{
		FileSystem: fs,
		Path:       jsii.String("/"),
		PosixUser:  posixUser,
		CreateAcl:  createAcl,
	})
	workerAP := awsefs.NewAccessPoint(c, jsii.String("Worker"), &awsefs.AccessPointProps{
		FileSystem: fs,
		Path:       jsii.String("/"),
		PosixUser:  posixUser,
		CreateAcl:  createAcl,
	})

	if !props.DisableBackup && props.FileSystem == nil {
		plan := awsbackup.BackupPlan_Daily35DayRetention(c, jsii.String("BackupPlan"), nil)
		plan.AddSelection(jsii.String("Selection"), &awsbackup.BackupSelectionOptions{
			Resources: &[]awsbackup.BackupResource{
				awsbackup.BackupResource_FromEfsFileSystem(fs),
			},
		})
	}

	return &GoBridgeEfsConfig{
		Construct:   c,
		fileSystem:  fs,
		controlAP:   controlAP,
		workerAP:    workerAP,
		securityGrp: sg,
		vpcSubnets:  subnetSelection,
	}
}

// FileSystem returns the EFS filesystem (created or imported).
func (c *GoBridgeEfsConfig) FileSystem() awsefs.IFileSystem { return c.fileSystem }

// ControlAccessPoint returns the access point intended for the RW control task.
func (c *GoBridgeEfsConfig) ControlAccessPoint() awsefs.AccessPoint { return c.controlAP }

// WorkerAccessPoint returns the access point intended for RO worker tasks.
func (c *GoBridgeEfsConfig) WorkerAccessPoint() awsefs.AccessPoint { return c.workerAP }

// SecurityGroup returns the security group attached to the mount targets.
func (c *GoBridgeEfsConfig) SecurityGroup() awsec2.SecurityGroup { return c.securityGrp }

// VpcSubnets returns the resolved subnet selection used for mount targets.
// Parent constructs use this to validate that the consuming ECS services
// run in the same subnet selection.
func (c *GoBridgeEfsConfig) VpcSubnets() *awsec2.SubnetSelection { return c.vpcSubnets }

// AccessPoint is a deprecated shim returning ControlAccessPoint to keep
// the legacy GoBridgeService construct compiling during the redesign.
//
// Deprecated: use ControlAccessPoint or WorkerAccessPoint.
func (c *GoBridgeEfsConfig) AccessPoint() awsefs.AccessPoint { return c.controlAP }
