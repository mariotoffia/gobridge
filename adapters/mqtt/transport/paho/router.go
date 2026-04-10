package paho

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/eclipse/paho.golang/packets"
	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/log"

	"github.com/mariotoffia/gobridge/logging"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// router implements paho.Router, dispatching incoming publishes to all
// registered handlers (one per Receiver on this Session). A WaitGroup
// tracks in-flight handler goroutines so Session.Close can await them.
//
// Messages arriving before any handler is registered are dropped. Use
// Session.Health().HandlersRegistered to confirm handlers are active
// before sending traffic. The DeepHealth / gobridgesync mechanism
// checks this automatically.
type router struct {
	mu         sync.RWMutex
	wg         sync.WaitGroup
	handlers   map[string]func(*pahov5.Publish)
	logger     *slog.Logger
	metrics    ports.MetricsExporter
	routeCount atomic.Int64 // total messages received by Route()
	dropCount  atomic.Int64 // messages dropped (no handler registered)
}

func newRouter(logger *slog.Logger, metrics ports.MetricsExporter) *router {
	if metrics == nil {
		metrics = &ports.NoopExporter{}
	}
	return &router{
		handlers: make(map[string]func(*pahov5.Publish)),
		logger:   logger,
		metrics:  metrics,
	}
}

// Route implements paho.Router. It converts packets.Publish to paho.Publish
// and dispatches to all registered handlers concurrently. Each handler
// receives an independent copy of the Publish -- both the struct and the
// Payload slice are copied so that handlers can safely inspect (but should
// not mutate) the data without racing on shared backing arrays.
//
// If no handlers are registered, the message is dropped and counted.
func (r *router) Route(pb *packets.Publish) {
	r.routeCount.Add(1)
	pub := pahov5.PublishFromPacketPublish(pb)
	r.mu.RLock()
	if len(r.handlers) == 0 {
		r.mu.RUnlock()
		r.dropCount.Add(1)
		r.metrics.Counter(domain.MetricMQTTRouterDropped, 1)
		if r.logger != nil && r.logger.Enabled(context.Background(), slog.LevelDebug) {
			r.logger.Log(context.Background(), slog.LevelDebug,
				"mqtt: dropped message (no handler registered)",
				"topic", pub.Topic,
			)
		}
		return
	}
	n := len(r.handlers)
	handlers := make([]func(*pahov5.Publish), 0, n)
	for _, h := range r.handlers {
		handlers = append(handlers, h)
	}
	r.mu.RUnlock()

	if logging.TraceEnabled(r.logger) {
		r.logger.Log(context.Background(), logging.LevelTrace,
			"mqtt: routing message",
			"topic", pub.Topic,
			"payload_len", len(pub.Payload),
			"handler_count", n,
		)
	}

	r.wg.Add(len(handlers))
	for i, h := range handlers {
		p := *pub
		if pub.Payload != nil {
			p.Payload = make([]byte, len(pub.Payload))
			copy(p.Payload, pub.Payload)
		}
		if len(handlers) > 1 && pub.Properties != nil && i > 0 {
			orig := pub.Properties
			cp := *orig
			if orig.User != nil {
				cp.User = make(pahov5.UserProperties, len(orig.User))
				copy(cp.User, orig.User)
			}
			if orig.CorrelationData != nil {
				cp.CorrelationData = make([]byte, len(orig.CorrelationData))
				copy(cp.CorrelationData, orig.CorrelationData)
			}
			if orig.SubscriptionIdentifier != nil {
				si := *orig.SubscriptionIdentifier
				cp.SubscriptionIdentifier = &si
			}
			if orig.PayloadFormat != nil {
				pf := *orig.PayloadFormat
				cp.PayloadFormat = &pf
			}
			if orig.MessageExpiry != nil {
				me := *orig.MessageExpiry
				cp.MessageExpiry = &me
			}
			if orig.TopicAlias != nil {
				ta := *orig.TopicAlias
				cp.TopicAlias = &ta
			}
			p.Properties = &cp
		}
		go func(handler func(*pahov5.Publish)) {
			defer r.wg.Done()
			defer func() {
				if rv := recover(); rv != nil {
					r.metrics.Counter(domain.MetricMQTTHandlerPanics, 1)
					if r.logger != nil {
						r.logger.Error("mqtt: handler panicked",
							"recovered", rv,
							"topic", p.Topic,
						)
					}
				}
			}()
			handler(&p)
		}(h)
	}
}

// Wait blocks until all in-flight handler goroutines have returned.
func (r *router) Wait() { r.wg.Wait() }

// Register adds a handler for the given ID. Handlers receive an
// independent copy of the Publish struct, Payload, and Properties per
// invocation when multiple handlers are registered. A single handler
// receives a struct copy with the original Properties pointer (no
// concurrent access risk). Handlers should treat Properties as read-only.
func (r *router) Register(id string, h func(*pahov5.Publish)) {
	r.mu.Lock()
	r.handlers[id] = h
	r.mu.Unlock()
	if logging.DebugEnabled(r.logger) {
		r.logger.Log(context.Background(), logging.LevelDebug, "mqtt: handler registered",
			"handler_id", id,
			"total_handlers", r.HandlerCount(),
		)
	}
}

func (r *router) Unregister(id string) {
	r.mu.Lock()
	delete(r.handlers, id)
	r.mu.Unlock()
	if logging.DebugEnabled(r.logger) {
		r.logger.Log(context.Background(), logging.LevelDebug, "mqtt: handler unregistered",
			"handler_id", id,
		)
	}
}

// HandlerCount returns the number of registered handlers.
func (r *router) HandlerCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.handlers)
}

// Stats returns diagnostic counters for the message router.
func (r *router) Stats() (received, dropped int64) {
	return r.routeCount.Load(), r.dropCount.Load()
}

// paho.Router interface stubs — registration is done via Register/Unregister.
func (r *router) RegisterHandler(_ string, _ pahov5.MessageHandler) {}
func (r *router) UnregisterHandler(_ string)                        {}
func (r *router) SetDebugLogger(_ log.Logger)                       {}
