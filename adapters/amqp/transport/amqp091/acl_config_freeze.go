package amqp091

import amqp "github.com/rabbitmq/amqp091-go"

// cloneSDKAMQPArgumentValue keeps the vendor-specific amqp.Table type inside
// the ACL while the adapter-owned freeze recursively clones its contents.
func cloneSDKAMQPArgumentValue(value any) (any, bool) {
	table, ok := value.(amqp.Table)
	if !ok {
		return nil, false
	}
	cloned := make(amqp.Table, len(table))
	for key, nested := range table {
		cloned[key] = cloneAMQPArgumentValue(nested)
	}
	return cloned, true
}
