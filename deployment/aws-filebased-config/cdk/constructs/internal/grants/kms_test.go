//go:build !race

package grants_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/awskms"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/grants"
)

func TestGrantKMSEfsCmkUse(t *testing.T) {
	stack, role := newTestStack(t)
	key := awskms.Key_FromKeyArn(stack, jsii.String("K"),
		jsii.String("arn:aws:kms:us-east-1:111122223333:key/abcd1234-12ab-34cd-56ef-1234567890ab"))

	grants.GrantKMSEfsCmkUse(role, key)

	actions := collectAllowActions(t, stack)
	mustHave(t, actions, "kms:Decrypt", "kms:GenerateDataKey", "kms:DescribeKey")
}
