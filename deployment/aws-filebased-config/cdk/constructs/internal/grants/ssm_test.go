//go:build !race

package grants_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/awskms"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsssm"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/grants"
)

func TestGrantSSMRead_SecureStringWithCMK(t *testing.T) {
	stack, role := newTestStack(t)
	key := awskms.Key_FromKeyArn(stack, jsii.String("K"),
		jsii.String("arn:aws:kms:us-east-1:111122223333:key/abcd1234-12ab-34cd-56ef-1234567890ab"))
	param := awsssm.StringParameter_FromSecureStringParameterAttributes(
		stack, jsii.String("P"), &awsssm.SecureStringParameterAttributes{
			ParameterName: jsii.String("/gobridge/secret"),
			EncryptionKey: key,
		})

	grants.GrantSSMRead(role, param)

	actions := collectAllowActions(t, stack)
	mustHave(t, actions,
		"ssm:DescribeParameters",
		"ssm:GetParameters",
		"ssm:GetParameter",
		"ssm:GetParameterHistory",
		"kms:Decrypt",
	)
}

func TestGrantSSMRead_PlainStringNoKMS(t *testing.T) {
	stack, role := newTestStack(t)
	param := awsssm.StringParameter_FromStringParameterName(
		stack, jsii.String("P"), jsii.String("/gobridge/plain"))

	grants.GrantSSMRead(role, param)

	actions := collectAllowActions(t, stack)
	mustHave(t, actions,
		"ssm:DescribeParameters",
		"ssm:GetParameters",
		"ssm:GetParameter",
		"ssm:GetParameterHistory",
	)
	mustNotHave(t, actions, "kms:Decrypt")
}
