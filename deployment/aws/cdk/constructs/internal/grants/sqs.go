package grants

import (
	"errors"
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
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

// GrantSQSConfig validates binding/session constraints, then resolves actual
// receiver/sender references through the same scoped QueueRegistry resolver used
// by synth validation. Data-plane grants always target exact queue handles.
// Queue dependencies are retained even when
// the serialized config contains only literal physical names or tag selectors.
// The caller must surface returned errors; missing/ambiguous selectors never
// fall back to wildcard data-plane grants.
func GrantSQSConfig(scope constructs.Construct, role awsiam.IGrantable, cfg *ports.BridgeConfig, queues *registry.QueueRegistry) error {
	if cfg == nil {
		return nil
	}
	// Validate the complete binding/session contract before emitting any IAM
	// statements. Bindings never construct an independent runtime sender.
	if err := bridgecfg.ValidateSQSConfig(cfg); err != nil {
		return err
	}
	var errs []error
	grant := func(id string, pc ports.PluginConfig, receiver bool) {
		var c sqs.Config
		switch v := pc.(type) {
		case *sqs.Config:
			if v == nil {
				errs = append(errs, fmt.Errorf("SQS %s requires a typed config", id))
				return
			}
			c = *v
		case sqs.Config:
			c = v
		default:
			errs = append(errs, fmt.Errorf("SQS %s requires a typed config", id))
			return
		}
		ref, err := queues.ResolveQueue(c, scope)
		if err != nil {
			errs = append(errs, fmt.Errorf("SQS %s: %w", id, err))
			return
		}
		if !ref.IsResolved() {
			return // Legacy explicit URL; caller manages IAM outside the registry.
		}
		queue := ref.Queue()
		if receiver {
			GrantSQSReceiver(role, queue, c.AutoExtendEnabled())
		} else {
			GrantSQSSender(role, queue)
		}
		if c.QueueTags != nil {
			grantSQSDiscovery(role, queue, c.QueueNamePrefix)
		}
		if scope != nil {
			scope.Node().AddDependency(queue)
		}
	}
	for _, receiver := range cfg.Receivers {
		if sqs.IsKind(receiver.Transport) {
			grant("receiver "+receiver.ID, receiver.Config, true)
		}
	}
	for _, sender := range cfg.Senders {
		if sqs.IsKind(sender.Transport) {
			grant("sender "+sender.ID, sender.Config, false)
		}
	}
	// The shared validator proves each binding refers to its sender's queue.
	// Grant only actual receiver/sender definitions, never ignored overrides.
	return errors.Join(errs...)
}

func grantSQSDiscovery(role awsiam.IGrantable, queue awssqs.IQueue, prefix string) {
	// ListQueues does not support resource-level IAM. ListQueueTags must
	// cover every scanned candidate, not just the selected data-plane queue;
	// otherwise a denied non-match would abort discovery.
	awsiam.Grant_AddToPrincipal(&awsiam.GrantOnPrincipalOptions{
		Grantee: role, Actions: jsii.Strings("sqs:ListQueues"), ResourceArns: jsii.Strings("*"),
	})
	metadataARN := awscdk.Stack_Of(queue).FormatArn(&awscdk.ArnComponents{
		Service: jsii.String("sqs"), Region: queue.Env().Region,
		Account: queue.Env().Account, Resource: jsii.String(prefix + "*"),
	})
	awsiam.Grant_AddToPrincipal(&awsiam.GrantOnPrincipalOptions{
		Grantee: role, Actions: jsii.Strings("sqs:ListQueueTags"), ResourceArns: &[]*string{metadataARN},
	})
}
