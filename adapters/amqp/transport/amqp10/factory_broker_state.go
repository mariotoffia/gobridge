package amqp10

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/mariotoffia/gobridge/ports"
)

// brokerStateKeyPrefix scopes an AMQP 1.0 broker state key, so it never equals
// a key another transport computes.
const brokerStateKeyPrefix = "amqp10:"

// BrokerStateKeys implements ports.BrokerStateKeyer (ADR 0024). A receiver with
// durability_mode above 0 on a multicast (topic) address holds a durable
// subscription the broker keeps under the container_id and the link name; its
// key is the canonical broker endpoint, the container_id and the effective link
// name, digested so the key carries none of them. A non-durable receiver and a
// queue (anycast) receiver hold no key, so GoBridge never deletes a queue.
func (f *Factory) BrokerStateKeys(session ports.SessionSpec, receivers []ports.ReceiverSpec) ([]string, error) {
	var keys []string
	for _, spec := range receivers {
		cfg, err := configFromSpec(spec.Config)
		if err != nil {
			return nil, fmt.Errorf("amqp10 receiver %q: %w", spec.ID, err)
		}
		if !holdsDurableTopicSubscription(cfg.Receiver) {
			continue
		}
		key, err := durableSubscriptionKey(session, cfg.Receiver)
		if err != nil {
			return nil, fmt.Errorf("amqp10 receiver %q: %w", spec.ID, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// holdsDurableTopicSubscription reports whether receiver attaches the link
// receiverLinkOptions makes a durable topic subscription: a durable source
// terminus with expiry policy never and the "topic" capability. The default
// routing is anycast, which attaches to a queue.
func holdsDurableTopicSubscription(receiver ReceiverParams) bool {
	return receiver.Routing == RoutingMulticast && receiver.DurabilityMode > 0
}

// durableSubscriptionKey returns the broker state key of one durable topic
// receiver on session. The effective link name is the one the receiver attaches
// with (receiverLinkName): the subscription_name, else the container_id and the
// address.
func durableSubscriptionKey(session ports.SessionSpec, receiver ReceiverParams) (string, error) {
	cfg, err := configFromSpec(session.Config)
	if err != nil {
		return "", fmt.Errorf("session %q: %w", session.ID, err)
	}
	containerID := cfg.Session.ContainerID
	if containerID == "" {
		return "", errors.New("a durable subscription needs an explicit session.container_id")
	}
	endpoint, err := canonicalBrokerEndpoint(cfg.Session.Address)
	if err != nil {
		return "", err
	}
	linkName := receiverLinkName(receiver.SubscriptionName, receiver.DurabilityMode, containerID, receiver.Address)
	digest := sha256.New()
	for _, part := range []string{endpoint, containerID, linkName} {
		_, _ = fmt.Fprintf(digest, "%d:%s;", len(part), part)
	}
	return brokerStateKeyPrefix + hex.EncodeToString(digest.Sum(nil)), nil
}

// canonicalBrokerEndpoint reduces a session address to the endpoint go-amqp
// dials, so two spellings of one broker compare equal: "amqp" and an empty
// scheme are one family, "amqps" and "amqp+ssl" the other, an omitted port is
// the family default (5672 or 5671), and host case, userinfo, path and query
// are ignored.
func canonicalBrokerEndpoint(address string) (string, error) {
	u, err := url.Parse(address)
	if err != nil || u.Hostname() == "" {
		return "", errors.New("a broker state key needs a session address with a host")
	}
	family, port := "amqp", "5672"
	switch strings.ToLower(u.Scheme) {
	case "amqps", "amqp+ssl":
		family, port = "amqps", "5671"
	case "", "amqp":
		// The defaults above.
	default:
		return "", fmt.Errorf("a broker state key cannot use address scheme %q", u.Scheme)
	}
	if p := u.Port(); p != "" {
		port = p
	}
	return family + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port), nil
}

var _ ports.BrokerStateKeyer = (*Factory)(nil)
