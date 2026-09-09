package registry

import (
	"fmt"
	"maps"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssqs"
)

// QueueRegistry maps logical SQS queue aliases to awssqs.IQueue handles.
// A registry alias is not necessarily the physical queue_name emitted in
// bridge config. Explicit tag bindings also retain the exact queue handle.
//
// QueueRegistry is not safe for concurrent use.
type QueueRegistry struct {
	queues    map[string]awssqs.IQueue
	selectors map[string]queueSelector
}

// NewQueueRegistry returns an empty QueueRegistry.
func NewQueueRegistry() *QueueRegistry {
	return &QueueRegistry{queues: map[string]awssqs.IQueue{}}
}

// AddQueue registers queue under the given logical name. Panics if
// name is empty, queue is nil, or name has already been registered —
// duplicates are programmer errors at synth time.
func (r *QueueRegistry) AddQueue(name string, queue awssqs.IQueue) {
	if name == "" {
		panic("registry: QueueRegistry.AddQueue: name must not be empty")
	}
	if queue == nil {
		panic(fmt.Sprintf("registry: QueueRegistry.AddQueue: queue for %q must not be nil", name))
	}
	if _, ok := r.queues[name]; ok {
		panic(fmt.Sprintf("registry: QueueRegistry.AddQueue: queue %q already registered", name))
	}
	r.queues[name] = queue
}

// Has reports whether name has been registered.
func (r *QueueRegistry) Has(name string) bool {
	_, ok := r.queues[name]
	return ok
}

// Names returns the logical names of all registered queues. Order is
// unspecified.
func (r *QueueRegistry) Names() []string {
	out := make([]string, 0, len(r.queues))
	for n := range r.queues {
		out = append(out, n)
	}
	return out
}

// Ref returns a QueueRef capturing the logical name and the
// underlying handle. If name has not been registered the returned
// ref reports IsResolved() == false; callers (typically the Phase 2
// validator) are expected to surface the miss via
// Annotations.addError.
func (r *QueueRegistry) Ref(name string) QueueRef {
	selector := r.selectors[name]
	return QueueRef{name: name, queue: r.queues[name], tags: maps.Clone(selector.tags), prefix: selector.prefix}
}

// QueueRef is a thin value-object referencing a registered SQS queue
// by logical name. The zero value is unresolved and has no name.
type QueueRef struct {
	name   string
	queue  awssqs.IQueue
	tags   map[string]string
	prefix string
}

// Name returns the logical queue name the ref was created for.
func (r QueueRef) Name() string { return r.name }

// Queue returns the underlying awssqs.IQueue handle, or nil when the
// ref is unresolved. Use IsResolved to disambiguate.
func (r QueueRef) Queue() awssqs.IQueue { return r.queue }

// IsResolved reports whether the ref carries a non-nil queue handle.
func (r QueueRef) IsResolved() bool { return r.queue != nil }

// PhysicalName returns the known physical QueueName, never a registry alias
// or a CloudFormation token. Empty means the name is not known during synth.
func (r QueueRef) PhysicalName() string {
	if r.queue == nil {
		return ""
	}
	name := r.queue.QueueName()
	// L2 QueueName is a Ref token even when QueueProps.QueueName was
	// explicit. The owned L1 property retains the actual synth-time input.
	if owned, ok := r.queue.Node().DefaultChild().(awssqs.CfnQueue); ok {
		name = owned.QueueName()
	}
	if name == nil || *awscdk.Token_IsUnresolved(name) {
		return ""
	}
	return *name
}

// QueueTags returns an isolated copy of the explicitly bound selector.
func (r QueueRef) QueueTags() map[string]string { return maps.Clone(r.tags) }

// QueueNamePrefix returns the optional discovery prefix bound to the selector.
func (r QueueRef) QueueNamePrefix() string { return r.prefix }
