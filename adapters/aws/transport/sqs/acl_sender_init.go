package sqs

import (
	"context"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

// ensureClient lazily creates the SDK SQS client for the sender and
// resolves the queue URL. Honours an injected fake (cfg.Client) when
// present. Failed discovery is not cached; the caller owns retries.
func (s *Sender) ensureClient(ctx context.Context) error {
	s.initMu.Lock()
	defer s.initMu.Unlock()

	client := s.loadClient()
	if client != nil && s.queueURL != "" {
		return nil
	}

	initCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	if client == nil {
		if s.cfg.Client != nil {
			client = s.cfg.Client
		} else if s.cfg.InitialCredentials != nil {
			c, err := rebuildSQSClient(initCtx, s.cfg.Region, s.cfg.Endpoint, s.cfg.Profile, s.cfg.InitialCredentials)
			if err != nil {
				return err
			}
			client = c
		} else {
			cfg, err := buildAWSConfig(initCtx, s.cfg.Region, s.cfg.Endpoint, s.cfg.Profile)
			if err != nil {
				return err
			}
			client = awssqs.NewFromConfig(cfg)
		}
		s.storeClient(client)
	}

	url, err := resolveQueueURL(initCtx, client, s.cfg.QueueURL, s.cfg.QueueName, s.cfg.QueueTags, s.cfg.QueueNamePrefix)
	if err != nil {
		return err
	}
	// A tag selector can discover FIFO only at runtime. Keep the same
	// fail-fast configuration rules as a URL/name-selected FIFO queue.
	if isFIFOQueue(url) {
		if s.cfg.DelaySeconds > 0 {
			return shared.ErrInvalidConfig.WithMessage("sqs: delay_seconds is not supported on FIFO queues")
		}
		if !s.cfg.FIFO && s.cfg.MessageGroupID == "" {
			return shared.ErrInvalidConfig.WithMessage("sqs: resolved FIFO queue requires message_group_id or fifo: true")
		}
	}
	s.queueURL = url

	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "sqs: sender initialized",
			"queue_url", s.queueURL,
			"region", s.cfg.Region,
		)
	}
	return nil
}
