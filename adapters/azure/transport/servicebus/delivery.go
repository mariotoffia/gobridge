package servicebus

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

var _ ports.Delivery = (*asbDelivery)(nil)

type asbDelivery struct {
	env    *domain.Envelope
	client asbAPI
	msg    *azservicebus.ReceivedMessage
	logger *slog.Logger
	cancel context.CancelFunc
	once   sync.Once
}

func newDelivery(
	parentCtx context.Context,
	env *domain.Envelope,
	client asbAPI,
	msg *azservicebus.ReceivedMessage,
	lockDuration time.Duration,
	autoExtend bool,
	logger *slog.Logger,
) *asbDelivery {
	ctx, cancel := context.WithCancel(parentCtx)

	d := &asbDelivery{
		env:    env,
		client: client,
		msg:    msg,
		logger: logger,
		cancel: cancel,
	}

	if autoExtend && lockDuration > 0 {
		interval := lockDuration / 2

		if msg.LockedUntil != nil && msg.LockedUntil.After(time.Now()) {
			remaining := time.Until(*msg.LockedUntil)
			interval = remaining / 2
		}

		if interval < time.Second {
			interval = time.Second
		}

		go d.autoExtendLoop(ctx, interval)
	}

	return d
}

func (d *asbDelivery) Envelope() *domain.Envelope { return d.env }

func (d *asbDelivery) Ack(ctx context.Context) error {
	d.stop()

	if err := d.client.CompleteMessage(ctx, d.msg, nil); err != nil {
		return MapError(err)
	}
	return nil
}

func (d *asbDelivery) Retry(ctx context.Context, _ time.Duration, _ error) error {
	d.stop()

	if err := d.client.AbandonMessage(ctx, d.msg, nil); err != nil {
		return MapError(err)
	}
	return nil
}

func (d *asbDelivery) Extend(ctx context.Context, _ time.Time) error {
	if err := d.client.RenewMessageLock(ctx, d.msg, nil); err != nil {
		return MapError(err)
	}
	return nil
}

func (d *asbDelivery) stop() {
	d.once.Do(func() {
		if d.cancel != nil {
			d.cancel()
		}
	})
}

func (d *asbDelivery) autoExtendLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.client.RenewMessageLock(ctx, d.msg, nil); err != nil {
				if ctx.Err() != nil {
					return
				}
				if d.logger != nil {
					d.logger.Warn("servicebus: auto-extend lock failed",
						"message_id", d.msg.MessageID,
						"error", err,
					)
				}
				return
			}
		}
	}
}
