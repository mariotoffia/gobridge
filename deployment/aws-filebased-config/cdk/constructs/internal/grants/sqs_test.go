//go:build !race

package grants_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/grants"
)

func TestGrantSQS(t *testing.T) {

	tests := []struct {
		name        string
		grant       func(role awsiam.IGrantable, q awssqs.IQueue)
		mustHave    []string
		mustNotHave []string
	}{
		{
			name: "receiver_no_auto_extend",
			grant: func(r awsiam.IGrantable, q awssqs.IQueue) {
				grants.GrantSQSReceiver(r, q, false)
			},
			mustHave: []string{
				"sqs:ReceiveMessage",
				"sqs:GetQueueUrl",
				"sqs:DeleteMessage",
				"sqs:GetQueueAttributes",
			},
			mustNotHave: []string{"sqs:ChangeMessageVisibility"},
		},
		{
			name: "receiver_auto_extend",
			grant: func(r awsiam.IGrantable, q awssqs.IQueue) {
				grants.GrantSQSReceiver(r, q, true)
			},
			mustHave: []string{
				"sqs:ReceiveMessage",
				"sqs:DeleteMessage",
				"sqs:GetQueueAttributes",
				"sqs:GetQueueUrl",
				"sqs:ChangeMessageVisibility",
			},
		},
		{
			name: "sender_only",
			grant: func(r awsiam.IGrantable, q awssqs.IQueue) {
				grants.GrantSQSSender(r, q)
			},
			mustHave: []string{"sqs:SendMessage", "sqs:GetQueueAttributes", "sqs:GetQueueUrl"},
			mustNotHave: []string{
				"sqs:ReceiveMessage",
				"sqs:DeleteMessage",
			},
		},
		{
			name: "receiver_idempotent_double_call",
			grant: func(r awsiam.IGrantable, q awssqs.IQueue) {
				grants.GrantSQSReceiver(r, q, false)
				grants.GrantSQSReceiver(r, q, false)
			},
			mustHave: []string{"sqs:ReceiveMessage"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stack, role := newTestStack(t)
			q := awssqs.Queue_FromQueueArn(stack, jsii.String("Q"),
				jsii.String("arn:aws:sqs:us-east-1:111122223333:my-queue"))
			tc.grant(role, q)
			actions := collectAllowActions(t, stack)
			mustHave(t, actions, tc.mustHave...)
			mustNotHave(t, actions, tc.mustNotHave...)
		})
	}
}

func TestGrantSQSReceiver_AutoExtendAddsChangeVisibility(t *testing.T) {
	baseActions := []string{
		"sqs:ReceiveMessage",
		"sqs:DeleteMessage",
		"sqs:GetQueueAttributes",
		"sqs:GetQueueUrl",
	}
	for _, ext := range []bool{false, true} {
		stack, role := newTestStack(t)
		q := awssqs.Queue_FromQueueArn(stack, jsii.String("Q"),
			jsii.String("arn:aws:sqs:us-east-1:111122223333:my-queue"))
		grants.GrantSQSReceiver(role, q, ext)
		actions := collectAllowActions(t, stack)
		mustHave(t, actions, baseActions...)
		if ext {
			mustHave(t, actions, "sqs:ChangeMessageVisibility")
		} else {
			mustNotHave(t, actions, "sqs:ChangeMessageVisibility")
		}
	}
}
