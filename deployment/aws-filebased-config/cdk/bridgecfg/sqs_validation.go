package bridgecfg

import (
	"fmt"
	"maps"

	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ValidateSQSConfig checks the SQS sender/binding and stateless-session contracts
// shared by embedding, synth validation and grants. QueueRegistry and
// QueueRegistry (embedded references) in
// deployment/aws-filebased-config/UBIQUITOUS.md define the exact-reference
// contract. No AWS or CDK calls are made.
//
// A binding cannot change its sender's queue selector or discovery scope:
// runtime constructs the sender only from SenderDef.Config. Partial binding
// options and identical selectors are allowed, but are not merged into a new
// sender. SQS receiver/sender connection attachments are rejected because SQS
// creates no Session. A binding's SessionID instead identifies its outbox/drainer
// owner and may refer to a stateful session independently of the sender transport.
func ValidateSQSConfig(cfg *ports.BridgeConfig) error {
	if cfg == nil {
		return nil
	}
	sessions := make(map[string]string, len(cfg.Sessions))
	for _, session := range cfg.Sessions {
		sessions[session.ID] = session.Transport
	}
	kind := func(transport, session string) string {
		if transport != "" {
			return transport
		}
		return sessions[session]
	}
	for _, receiver := range cfg.Receivers {
		if err := validateSQSEndpoint("receiver", receiver.ID, kind(receiver.Transport, receiver.SessionID), receiver.SessionID, receiver.Config); err != nil {
			return err
		}
	}
	senders := make(map[string]ports.SenderDef, len(cfg.Senders))
	for _, sender := range cfg.Senders {
		if err := validateSQSEndpoint("sender", sender.ID, kind(sender.Transport, sender.SessionID), sender.SessionID, sender.Config); err != nil {
			return err
		}
		senders[sender.ID] = sender
	}
	for _, binding := range cfg.Bindings {
		sender, exists := senders[binding.SenderID]
		if !exists || !sqs.IsKind(kind(sender.Transport, sender.SessionID)) {
			if isSQSConfig(binding.Config) || sqs.IsKind(sessions[binding.SessionID]) {
				return sqsConfigError("binding", binding.ID, "requires an existing SQS sender")
			}
			continue
		}
		if sqs.IsKind(sessions[binding.SessionID]) {
			return sqsConfigError("binding", binding.ID, "SQS is stateless; binding session_id must name a stateful outbox/drainer owner")
		}
		if ports.IsNilPluginConfig(binding.Config) && binding.Raw() == nil {
			continue
		}
		bc, ok := sqsConfigValue(binding.Config)
		if !ok {
			return sqsConfigError("binding", binding.ID, "requires a decoded sqs.Config")
		}
		if err := bc.Validate(); err != nil {
			return fmt.Errorf("SQS binding %q: %w", binding.ID, shared.ErrInvalidConfig.Wrap(err))
		}
		sc, _ := sqsConfigValue(sender.Config) // validated above
		if !sameSQSQueue(sc, bc) ||
			(bc.Region != "" && bc.Region != sc.Region) ||
			(bc.Endpoint != "" && bc.Endpoint != sc.Endpoint) ||
			(bc.Profile != "" && bc.Profile != sc.Profile) ||
			(bc.CredentialsURIRef != "" && bc.CredentialsURIRef != sc.CredentialsURIRef) {
			return sqsConfigError("binding", binding.ID,
				"must not change its sender's queue reference or discovery scope; runtime uses only SenderDef.Config")
		}
	}
	return nil
}

func validateSQSEndpoint(surface, id, kind, session string, pc ports.PluginConfig) error {
	if !sqs.IsKind(kind) {
		if isSQSConfig(pc) {
			return sqsConfigError(surface, id, "requires a resolved sqs or aws.sqs transport")
		}
		return nil
	}
	if session != "" {
		return sqsConfigError(surface, id, "SQS is stateless; remove session_id and declare transport explicitly")
	}
	c, ok := sqsConfigValue(pc)
	if !ok {
		return sqsConfigError(surface, id, "requires a decoded sqs.Config")
	}
	if err := c.Validate(); err != nil {
		return fmt.Errorf("SQS %s %q: %w", surface, id, shared.ErrInvalidConfig.Wrap(err))
	}
	if err := c.ValidateQueue(); err != nil {
		return fmt.Errorf("SQS %s %q: %w", surface, id, err)
	}
	return nil
}

func sameSQSQueue(sender, binding sqs.Config) bool {
	switch {
	case binding.QueueURL != "":
		return binding.QueueURL == sender.QueueURL
	case binding.QueueName != "":
		return sender.QueueURL == "" && binding.QueueName == sender.QueueName
	case binding.QueueTags != nil:
		return sender.QueueURL == "" && sender.QueueName == "" &&
			maps.Equal(binding.QueueTags, sender.QueueTags) && binding.QueueNamePrefix == sender.QueueNamePrefix
	default:
		return true // No queue-reference override.
	}
}

func sqsConfigValue(pc ports.PluginConfig) (sqs.Config, bool) {
	switch c := pc.(type) {
	case *sqs.Config:
		if c != nil {
			return *c, true
		}
	case sqs.Config:
		return c, true
	}
	return sqs.Config{}, false
}

func isSQSConfig(pc ports.PluginConfig) bool {
	return !ports.IsNilPluginConfig(pc) && sqs.IsKind(pc.Kind())
}

func sqsConfigError(surface, id, message string) error {
	return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf("SQS %s %q: %s", surface, id, message))
}
