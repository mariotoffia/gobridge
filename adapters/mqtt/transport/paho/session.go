package paho

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sync"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/ports"
)

// Session implements ports.Session for MQTT, owning the broker connection,
// ClientID identity, and subscription reconciliation.
type Session struct {
	opts    SessionOptions
	mode    domain.SessionMode
	logger  *slog.Logger
	metrics ports.MetricsExporter
	clk     clock.Clock

	mu        sync.Mutex
	cm        *autopaho.ConnectionManager
	cmCancel  context.CancelFunc // cancels the CM's background context on Close
	events    chan ports.SessionEvent
	closed    bool
	connected bool // true when autopaho reports connection is up
	starting  bool // guards against concurrent Start() calls (BUG-2 fix)

	// reconcileMu serializes concurrent reconcile calls (e.g. external
	// Reconcile vs OnConnectionUp callback) to prevent interleaved
	// subscribe/unsubscribe operations from corrupting activeSubs.
	reconcileMu sync.Mutex

	// router receives all incoming publishes; Receivers register handlers.
	router *router

	// plan is the last reconciled session plan, re-applied on reconnect.
	plan *domain.SessionPlan

	// activeSubs tracks topics for which SUBSCRIBE has been issued.
	activeSubs map[string]byte // topic -> qos

	// startCtx is a long-lived context derived from context.Background(),
	// used to derive reconnect reconciliation contexts. It is cancelled
	// by Close() via cmCancel, ensuring in-progress reconciliations are
	// aborted on shutdown.
	startCtx context.Context

	// liveCreds is the most recently applied credential material. It is
	// consulted by the ConnectPacketBuilder on every (re)connect so that
	// rotated credentials take effect on the NEXT CONNECT packet
	// without rebuilding the ConnectionManager. Protected by mu.
	liveCreds mqttCredentials
}

// mqttCredentials is the mutable subset of SessionOptions that can be
// rotated at runtime. TLS material is intentionally out of scope for
// now — rotating a TLS certificate requires a new TLS handshake which
// autopaho does not support on an existing CM.
type mqttCredentials struct {
	Username string
	Password string
}

var _ ports.Session = (*Session)(nil)

// NewSession creates an MQTT Session from the given options.
// metrics may be nil; a no-op exporter is used in that case.
func NewSession(opts SessionOptions, mode domain.SessionMode, logger *slog.Logger, metrics ...ports.MetricsExporter) *Session {
	var m ports.MetricsExporter = &ports.NoopExporter{}
	if len(metrics) > 0 && metrics[0] != nil {
		m = metrics[0]
	}
	if opts.Clock == nil {
		opts.Clock = clock.System
	}
	return &Session{
		opts:       opts,
		mode:       mode,
		logger:     logger,
		metrics:    m,
		clk:        opts.Clock,
		events:     make(chan ports.SessionEvent, 16),
		router:     newRouter(logger, m),
		activeSubs: make(map[string]byte),
	}
}

// ConnectionManager returns the underlying autopaho.ConnectionManager.
// Receiver and Sender use this to issue subscribe/publish calls.
func (s *Session) clock() clock.Clock {
	if s.clk != nil {
		return s.clk
	}
	return clock.System
}

func (s *Session) ConnectionManager() *autopaho.ConnectionManager {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cm
}

// Router returns the message router for registering Receiver handlers.
func (s *Session) Router() *router {
	return s.router
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// classifySubackReasons walks the SUBACK reason codes one-to-one with
// the requested subscriptions and partitions them into accepted and
// rejected. The first rejection's classified BridgeError is returned
// alongside the offending topic so the caller can surface a meaningful
// failure, but EVERY accepted topic is included in the succeeded slice
// so the caller can persist a faithful view of broker state.
//
// Topics whose reason index is out of range (broker returned a short
// SUBACK) are conservatively treated as accepted — matching the
// previous implementation and avoiding gratuitous unsubscribe loops.
func classifySubackReasons(toSub []pahov5.SubscribeOptions, reasons []byte) (
	succeeded []pahov5.SubscribeOptions, firstErr *domain.BridgeError, errTopic string,
) {
	succeeded = make([]pahov5.SubscribeOptions, 0, len(toSub))
	for i, opt := range toSub {
		if i < len(reasons) {
			if berr := MapSubscribeReasonCode(reasons[i]); berr != nil {
				if firstErr == nil {
					firstErr = berr
					errTopic = opt.Topic
				}
				continue
			}
		}
		succeeded = append(succeeded, opt)
	}
	return succeeded, firstErr, errTopic
}

func parseURLs(raw []string) ([]*url.URL, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("at least one broker URL is required")
	}
	out := make([]*url.URL, len(raw))
	for i, s := range raw {
		u, err := url.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("invalid broker URL %q: %w", s, err)
		}
		out[i] = u
	}
	return out, nil
}
