package grants

import (
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/jsii-runtime-go"
)

// GrantSQSReceiver grants the principal the actions required to consume
// messages from queue. When autoExtend is true the principal is also
// granted sqs:ChangeMessageVisibility so a receiver can extend the
// visibility timeout of an in-flight message.
//
// Idempotent: repeated calls collapse into a single IAM statement.
func GrantSQSReceiver(role awsiam.IGrantable, queue awssqs.IQueue, autoExtend bool) {
	queue.Grant(role,
		jsii.String("sqs:ReceiveMessage"),
		jsii.String("sqs:DeleteMessage"),
		jsii.String("sqs:GetQueueAttributes"),
		jsii.String("sqs:GetQueueUrl"),
	)
	if autoExtend {
		queue.Grant(role, jsii.String("sqs:ChangeMessageVisibility"))
	}
}

// GrantSQSSender grants the principal the actions required to send
// messages to queue. No FIFO-specific extras are added; CDK already
// emits the correct action set for both standard and FIFO queues via
// GrantSendMessages.
func GrantSQSSender(role awsiam.IGrantable, queue awssqs.IQueue) {
	queue.GrantSendMessages(role)
}
