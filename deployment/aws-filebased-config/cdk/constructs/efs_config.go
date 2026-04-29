// Package constructs provides reusable CDK L2 constructs for deploying
// the gobridge file-based configuration profile on AWS ECS Fargate.
package constructs

import (
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsefs"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
)

// GoBridgeEfsConfigProps configures the EFS filesystem and access point
// used to mount the gobridge bridge configuration file.
type GoBridgeEfsConfigProps struct {
	// Vpc is the VPC in which EFS mount targets are created.
	Vpc awsec2.IVpc

	// FileSystem is an existing EFS filesystem to reuse.
	// If nil, a new filesystem is created.
	FileSystem awsefs.IFileSystem

	// AccessPointPath is the POSIX path inside EFS for the gobridge config.
	// Default: /gobridge
	AccessPointPath *string

	// PosixUID is the POSIX user ID for the access point. Default: "1000"
	PosixUID *string

	// PosixGID is the POSIX group ID for the access point. Default: "1000"
	PosixGID *string

	// RemovalPolicy controls what happens to the filesystem on stack
	// deletion. Default: RETAIN
	RemovalPolicy interface{}
}

// GoBridgeEfsConfig is an L2 construct that creates an EFS filesystem with
// an access point pre-configured for the gobridge config mount path.
type GoBridgeEfsConfig struct {
	constructs.Construct

	fileSystem  awsefs.IFileSystem
	accessPoint awsefs.AccessPoint
	securityGrp awsec2.SecurityGroup
}

// NewGoBridgeEfsConfig creates a new EFS configuration construct.
func NewGoBridgeEfsConfig(scope constructs.Construct, id *string, props *GoBridgeEfsConfigProps) *GoBridgeEfsConfig {
	c := constructs.NewConstruct(scope, id)

	apPath := jsii.String("/gobridge")
	if props.AccessPointPath != nil {
		apPath = props.AccessPointPath
	}
	posixUID := jsii.String("1000")
	if props.PosixUID != nil {
		posixUID = props.PosixUID
	}
	posixGID := jsii.String("1000")
	if props.PosixGID != nil {
		posixGID = props.PosixGID
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
		fs = awsefs.NewFileSystem(c, jsii.String("Fs"), &awsefs.FileSystemProps{
			Vpc:             props.Vpc,
			Encrypted:       jsii.Bool(true),
			SecurityGroup:   sg,
			PerformanceMode: awsefs.PerformanceMode_GENERAL_PURPOSE,
		})
	}

	ap := awsefs.NewAccessPoint(c, jsii.String("AP"), &awsefs.AccessPointProps{
		FileSystem: fs,
		Path:       apPath,
		PosixUser: &awsefs.PosixUser{
			Uid: posixUID,
			Gid: posixGID,
		},
		CreateAcl: &awsefs.Acl{
			OwnerUid:    posixUID,
			OwnerGid:    posixGID,
			Permissions: jsii.String("755"),
		},
	})

	return &GoBridgeEfsConfig{
		Construct:   c,
		fileSystem:  fs,
		accessPoint: ap,
		securityGrp: sg,
	}
}

// FileSystem returns the EFS filesystem.
func (c *GoBridgeEfsConfig) FileSystem() awsefs.IFileSystem { return c.fileSystem }

// AccessPoint returns the EFS access point.
func (c *GoBridgeEfsConfig) AccessPoint() awsefs.AccessPoint { return c.accessPoint }

// SecurityGroup returns the security group for EFS mount targets.
func (c *GoBridgeEfsConfig) SecurityGroup() awsec2.SecurityGroup { return c.securityGrp }
