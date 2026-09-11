package registry

import (
	"fmt"
	"maps"
	"slices"

	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsssm"
)

// QueueTags selects a queue at runtime by its tags instead of its name. The
// queue itself must be listed under the same key in the construct's Queues.
type QueueTags struct {
	// Tags the runtime matches on the queue. Required; literal values only.
	Tags map[string]string
	// NamePrefix optionally narrows discovery to queue names with this prefix.
	NamePrefix string
}

// FromProps builds the registries a construct validates and grants against from
// its plain Queues, QueueTags and Secrets props. An empty map gives a nil
// registry, so a config that needs one still reports everything it is missing.
// A nil queue or parameter, or two secret keys naming the same path, panic as
// AddQueue and AddParameter do; a tag selector that cannot bind is an error.
func FromProps(
	queues map[string]awssqs.IQueue,
	tags map[string]QueueTags,
	secrets map[string]awsssm.IParameter,
) (*QueueRegistry, *SsmParamRegistry, error) {
	var queueRegistry *QueueRegistry
	if len(queues) > 0 {
		queueRegistry = NewQueueRegistry()
		for _, name := range slices.Sorted(maps.Keys(queues)) {
			queueRegistry.AddQueue(name, queues[name])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(tags)) {
		if queueRegistry == nil || !queueRegistry.Has(name) {
			return nil, nil, fmt.Errorf("QueueTags[%q] has no queue under the same key in Queues", name)
		}
		if err := queueRegistry.BindQueueTags(name, tags[name].Tags, tags[name].NamePrefix); err != nil {
			return nil, nil, fmt.Errorf("QueueTags[%q]: %w", name, err)
		}
	}
	var secretRegistry *SsmParamRegistry
	if len(secrets) > 0 {
		secretRegistry = NewSsmParamRegistry()
		for _, path := range slices.Sorted(maps.Keys(secrets)) {
			secretRegistry.AddParameter(path, secrets[path])
		}
	}
	return queueRegistry, secretRegistry, nil
}
