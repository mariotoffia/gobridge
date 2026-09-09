package bridgecfg

import (
	"fmt"
	"strings"

	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ValidateEmbeddedSQSConfig checks SQS references before embedding a materialized
// bridge config. Embedded initial config uses physical queue_name or queue_tags,
// never queue_url (even a literal URL or a URL paired with a name). See
// deployment/aws-filebased-config/UBIQUITOUS.md, QueueRegistry (embedded references).
//
// Transport inheritance follows the parser: a receiver/sender may use its
// session's transport, and a binding uses its sender's effective transport.
// Queue options are not inherited from sessions. SQS connection attachments are
// rejected as stateless; binding SessionID may name a separate stateful outbox
// owner. Bindings cannot select a different queue from their sender; only
// identical selectors or partial non-reference options are allowed.
// Non-empty SQS binding addresses must be physical names or sqs.QueueAddress.
//
// This is an offline, read-only reference and stateless-attachment check, not a
// full blueprint validator. It requires decoded sqs.Config payloads, performs no AWS
// calls or token resolution, and does not change non-embedded URL support.
// The embedding caller remains responsible for rejecting unresolved CDK tokens
// throughout the complete document. A nil config has nothing to validate.
func ValidateEmbeddedSQSConfig(cfg *ports.BridgeConfig) error {
	if cfg == nil {
		return nil
	}
	sessionKinds := make(map[string]string, len(cfg.Sessions))
	for _, session := range cfg.Sessions {
		sessionKinds[session.ID] = session.Transport
		if err := validateEmbeddedSQSOptions("session", session.ID, session.Transport, session.Config, session.Raw(), false); err != nil {
			return err
		}
	}
	effectiveKind := func(kind, sessionID string) string {
		if kind != "" {
			return kind
		}
		return sessionKinds[sessionID]
	}
	for _, receiver := range cfg.Receivers {
		kind := effectiveKind(receiver.Transport, receiver.SessionID)
		if err := validateEmbeddedSQSOptions("receiver", receiver.ID, kind, receiver.Config, receiver.Raw(), true); err != nil {
			return err
		}
		// Subscription options also serialize into an embedded receiver.
		// SQS has no independent subscription queue-resolution mechanism.
		for i, topic := range receiver.Topics {
			id := fmt.Sprintf("%s/topics[%d]", receiver.ID, i)
			if err := validateEmbeddedSQSOptions("receiver subscription", id, kind, topic.Config, topic.Raw(), false); err != nil {
				return err
			}
		}
	}
	senderKinds := make(map[string]string, len(cfg.Senders))
	for _, sender := range cfg.Senders {
		kind := effectiveKind(sender.Transport, sender.SessionID)
		senderKinds[sender.ID] = kind
		if err := validateEmbeddedSQSOptions("sender", sender.ID, kind, sender.Config, sender.Raw(), true); err != nil {
			return err
		}
	}
	for _, binding := range cfg.Bindings {
		kind := senderKinds[binding.SenderID]
		if err := validateEmbeddedSQSOptions("binding", binding.ID, kind, binding.Config, binding.Raw(), false); err != nil {
			return err
		}
		if sqs.IsKind(kind) && binding.Address != "" && binding.Address != sqs.QueueAddress && !embeddedSQSQueueName(binding.Address) {
			return embeddedSQSError("binding", binding.ID, "address must be a physical queue name or sqs:queue; URL and other address forms are not supported")
		}
	}
	return ValidateSQSConfig(cfg)
}

func validateEmbeddedSQSOptions(surface, id, kind string, pc ports.PluginConfig, raw ports.RawConfig, required bool) error {
	if !sqs.IsKind(kind) {
		if !ports.IsNilPluginConfig(pc) && sqs.IsKind(pc.Kind()) {
			return embeddedSQSError(surface, id, "SQS options require a resolved sqs or aws.sqs transport")
		}
		return nil
	}
	if ports.IsNilPluginConfig(pc) {
		if !required && raw == nil {
			return nil
		}
		return embeddedSQSError(surface, id, "requires a decoded sqs.Config; materialize the config before embedding")
	}
	var c sqs.Config
	switch value := pc.(type) {
	case *sqs.Config:
		c = *value
	case sqs.Config:
		c = value
	default:
		return embeddedSQSError(surface, id, "requires a decoded sqs.Config; unsupported SQS options type")
	}
	if c.QueueURL != "" {
		return embeddedSQSError(surface, id, "queue_url is not supported in embedded config; use queue_name or queue_tags")
	}
	if err := c.Validate(); err != nil {
		return fmt.Errorf("bridgecfg: embedded SQS %s %q: %w", surface, id, shared.ErrInvalidConfig.Wrap(err))
	}
	if c.QueueName != "" && !embeddedSQSQueueName(c.QueueName) {
		return embeddedSQSError(surface, id, "requires a physical queue_name of 1–80 characters: letters, digits, hyphens or underscores, optionally ending in .fifo")
	}
	if required && c.QueueName == "" && c.QueueTags == nil {
		return embeddedSQSError(surface, id, "requires queue_name or queue_tags on its own options; session queue options are not inherited")
	}
	return nil
}

func embeddedSQSQueueName(name string) bool {
	if len(name) == 0 || len(name) > 80 {
		return false
	}
	base := strings.TrimSuffix(name, ".fifo")
	if base == "" {
		return false
	}
	for _, ch := range base {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z',
			ch >= '0' && ch <= '9', ch == '-', ch == '_':
		default:
			return false
		}
	}
	return true
}

func embeddedSQSError(surface, id, message string) error {
	return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf("bridgecfg: embedded SQS %s %q: %s", surface, id, message))
}
