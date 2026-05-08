package grants

import (
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsssm"
)

// GrantSSMRead grants the principal read access to param.
//
// CDK's IParameter.GrantRead emits ssm:DescribeParameters,
// ssm:GetParameters, ssm:GetParameter and ssm:GetParameterHistory.
// When param is a SecureString backed by a customer-managed KMS key,
// CDK additionally emits kms:Decrypt scoped to that key. SecureString
// parameters using the AWS-managed alias/aws/ssm key do not require an
// explicit kms:Decrypt statement (the key policy already permits it).
func GrantSSMRead(role awsiam.IGrantable, param awsssm.IParameter) {
	param.GrantRead(role)
}
