package sqs

import (
	"unicode/utf8"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// QueueAddress is the binding Address meaning "use the sender's configured
// queue". It is useful when QueueTags selects a queue whose physical name is
// unknown until initialization. It never enables per-message queue discovery.
const QueueAddress = "sqs:queue"

func validateQueueReference(url, name string, tags map[string]string, prefix string, required bool) error {
	invalid := func(message string) error {
		return shared.ErrInvalidConfig.WithMessage("sqs: " + message)
	}
	if tags == nil {
		if prefix != "" {
			return invalid("queue_name_prefix requires queue_tags")
		}
		if required && url == "" && name == "" {
			return invalid("either queue_url or queue_name or queue_tags is required")
		}
		return nil
	}
	if url != "" || name != "" {
		return invalid("queue_tags is an alternative to queue_url and queue_name")
	}
	if len(tags) == 0 || len(tags) > 50 {
		return invalid("queue_tags must contain between 1 and 50 tags")
	}
	for key, value := range tags {
		if key == "" || !utf8.ValidString(key) || utf8.RuneCountInString(key) > 128 {
			return invalid("queue_tags key must contain 1 to 128 UTF-8 characters")
		}
		if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 256 {
			return invalid("queue_tags value must contain at most 256 UTF-8 characters")
		}
	}
	if len(prefix) > 80 {
		return invalid("queue_name_prefix must contain at most 80 characters")
	}
	for _, ch := range prefix {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z',
			ch >= '0' && ch <= '9', ch == '-', ch == '_', ch == '.':
		default:
			return invalid("queue_name_prefix must contain only letters, numbers, hyphens, underscores or periods")
		}
	}
	return nil
}
