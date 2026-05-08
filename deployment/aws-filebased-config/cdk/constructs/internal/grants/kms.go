package grants

import (
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awskms"
	"github.com/aws/jsii-runtime-go"
)

// GrantKMSEfsCmkUse grants the principal the KMS actions required to
// mount an EFS file system encrypted with a customer-managed key.
//
// Action set: kms:Decrypt, kms:GenerateDataKey, kms:DescribeKey scoped
// to key. This helper is only invoked from the GoBridge facades when
// the EfsKmsKey prop is set; for AWS-managed keys EFS handles
// permissions implicitly via the key policy.
func GrantKMSEfsCmkUse(role awsiam.IGrantable, key awskms.IKey) {
	key.Grant(role,
		jsii.String("kms:Decrypt"),
		jsii.String("kms:GenerateDataKey"),
		jsii.String("kms:DescribeKey"),
	)
}
