package grants

import (
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
)

// GrantLogsWrite grants the principal write access to lg.
//
// CDK's ILogGroup.GrantWrite emits logs:CreateLogStream and
// logs:PutLogEvents on the log group ARN — exactly the action set the
// awslogs ECS log driver requires.
func GrantLogsWrite(role awsiam.IGrantable, lg awslogs.ILogGroup) {
	lg.GrantWrite(role)
}
