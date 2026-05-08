//go:build !race

package grants_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/grants"
)

func TestGrantLogsWrite(t *testing.T) {
	defer jsii.Close()
	stack, role := newTestStack(t)
	lg := awslogs.LogGroup_FromLogGroupArn(stack, jsii.String("LG"),
		jsii.String("arn:aws:logs:us-east-1:111122223333:log-group:/gobridge/test:*"))

	grants.GrantLogsWrite(role, lg)

	actions := collectAllowActions(t, stack)
	mustHave(t, actions, "logs:CreateLogStream", "logs:PutLogEvents")
}
