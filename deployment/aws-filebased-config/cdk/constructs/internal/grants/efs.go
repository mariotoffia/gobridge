package grants

import (
	"github.com/aws/aws-cdk-go/awscdk/v2/awsefs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/jsii-runtime-go"
)

// GrantEFSControl grants the principal read-write access to fs scoped
// to the access point ap. Used for the GoBridge control task role.
//
// IAM action set: elasticfilesystem:ClientMount + ClientWrite. The
// access-point ARN is added as a condition on the policy statement so
// the principal can only mount/write through the supplied access point
// even when other access points exist on the same file system.
func GrantEFSControl(role awsiam.IGrantable, fs awsefs.IFileSystem, ap awsefs.IAccessPoint) {
	grantEFS(role, fs, ap, jsii.Strings(
		"elasticfilesystem:ClientMount",
		"elasticfilesystem:ClientWrite",
	))
}

// GrantEFSWorker grants the principal read-only mount access to fs
// scoped to the access point ap. Used for the GoBridge worker task
// role (workers boot from EFS but never write).
//
// IAM action set: elasticfilesystem:ClientMount only — ClientWrite is
// deliberately omitted so workers cannot mutate cluster state even if
// the ECS volume readOnly flag were misconfigured.
func GrantEFSWorker(role awsiam.IGrantable, fs awsefs.IFileSystem, ap awsefs.IAccessPoint) {
	grantEFS(role, fs, ap, jsii.Strings("elasticfilesystem:ClientMount"))
}

// grantEFS adds a single IAM statement to the principal's inline
// policy: actions on the file system ARN, conditioned on the access
// point ARN via elasticfilesystem:AccessPointArn.
func grantEFS(role awsiam.IGrantable, fs awsefs.IFileSystem, ap awsefs.IAccessPoint, actions *[]*string) {
	awsiam.Grant_AddToPrincipal(&awsiam.GrantOnPrincipalOptions{
		Grantee:      role,
		Actions:      actions,
		ResourceArns: &[]*string{fs.FileSystemArn()},
		Conditions: &map[string]*map[string]interface{}{
			"StringEquals": {
				"elasticfilesystem:AccessPointArn": ap.AccessPointArn(),
			},
		},
	})
}
