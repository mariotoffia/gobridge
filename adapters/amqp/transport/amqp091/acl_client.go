package amqp091

import (
	"fmt"
	"net/url"

	amqp "github.com/rabbitmq/amqp091-go"
)

// amqpConnection is the unexported façade over *amqp091.Connection. All
// methods return either domain types or unexported wrappers (*amqpChannel,
// <-chan error) so that consumers outside this ACL never reference SDK
// types and never need to import "github.com/rabbitmq/amqp091-go".
type amqpConnection interface {
	// Channel opens a fresh AMQP channel, returned as an unexported
	// wrapper that exposes only domain-typed operations.
	Channel() (*amqpChannel, error)
	// Close terminates the underlying connection.
	Close() error
	// NotifyClose returns a single-use channel that emits at most one
	// error (the connection-close cause) and is then closed. Graceful
	// closes deliver no value before closing.
	NotifyClose() <-chan error
	// NotifyBlocked returns a stream of connection block/unblock
	// notifications the broker sends (connection.blocked /
	// connection.unblocked) when a resource alarm — memory or disk —
	// engages or clears TCP backpressure. Observing it lets the session
	// distinguish broker pushback from ordinary send timeouts. The
	// channel is closed when the underlying connection closes.
	NotifyBlocked() <-chan connBlockState
	// IsClosed reports whether the connection has been closed.
	IsClosed() bool
}

// connBlockState is the domain-typed mirror of amqp.Blocking. Active is
// true while the broker has TCP backpressure engaged; Reason carries the
// server-supplied cause (e.g. "low on memory"). Kept SDK-free so the
// Session never references amqp.Blocking.
type connBlockState struct {
	Active bool
	Reason string
}

// sdkConnection adapts a real *amqp091.Connection to amqpConnection so
// the rest of the package never touches the SDK type directly.
type sdkConnection struct {
	raw *amqp.Connection
}

// wrapConnection wraps a *amqp091.Connection as amqpConnection.
func wrapConnection(c *amqp.Connection) amqpConnection {
	return &sdkConnection{raw: c}
}

func (c *sdkConnection) Channel() (*amqpChannel, error) {
	ch, err := c.raw.Channel()
	if err != nil {
		return nil, fmt.Errorf("amqp091: open channel: %w", err)
	}
	return &amqpChannel{raw: ch}, nil
}

func (c *sdkConnection) Close() error {
	if err := c.raw.Close(); err != nil {
		return fmt.Errorf("amqp091: close connection: %w", err)
	}
	return nil
}
func (c *sdkConnection) IsClosed() bool { return c.raw.IsClosed() }

func (c *sdkConnection) NotifyClose() <-chan error {
	in := c.raw.NotifyClose(make(chan *amqp.Error, 1))
	out := make(chan error, 1)
	go func() {
		defer close(out)
		e, ok := <-in
		if !ok {
			return
		}
		if e != nil {
			out <- e
		}
	}()
	return out
}

// NotifyBlocked adapts amqp091-go's connection.blocked/unblocked stream to
// the SDK-free connBlockState channel. The forwarding goroutine exits when
// the SDK closes its channel on connection teardown.
func (c *sdkConnection) NotifyBlocked() <-chan connBlockState {
	in := c.raw.NotifyBlocked(make(chan amqp.Blocking, 1))
	out := make(chan connBlockState, 1)
	go func() {
		defer close(out)
		for b := range in {
			out <- connBlockState{Active: b.Active, Reason: b.Reason}
		}
	}()
	return out
}

// dialFunc abstracts the AMQP dial operation for test-double injection.
type dialFunc func(url string) (amqpConnection, error)

// dialConfig builds the amqp.Config used for every (re)dial. It carries
// the heartbeat interval and the AMQP virtual host.
//
// Vhost precedence: a non-empty SessionOptions.Vhost is forwarded to
// amqp091-go's Config.Vhost, which DialConfig honours over whatever path
// the broker URL encodes (DialConfig only falls back to the URL path when
// Config.Vhost == ""). Setting the field is the SDK-correct way to select
// the vhost and avoids the URL path-escaping pitfalls of names such as
// "/production" (which must be percent-encoded in a URI but not here). An
// empty Vhost preserves the historical behaviour of resolving the vhost
// from the broker URL.
func dialConfig(opts SessionOptions) amqp.Config {
	return amqp.Config{
		Heartbeat: opts.Heartbeat,
		Vhost:     opts.Vhost,
	}
}

// defaultDialFromOpts returns a dialFunc that honours TLS, heartbeat, and
// virtual-host configuration. When TLS is enabled, it builds a *tls.Config
// and dials with it; otherwise it uses amqp091.DialConfig with the
// configured heartbeat interval and vhost.
func defaultDialFromOpts(opts SessionOptions) dialFunc {
	cfg := dialConfig(opts)
	if opts.TLS != nil && opts.TLS.Enable {
		return func(brokerURL string) (amqpConnection, error) {
			tlsCfg, err := BuildTLSConfig(opts.TLS)
			if err != nil {
				return nil, err
			}
			cfg.TLSClientConfig = tlsCfg
			c, err := amqp.DialConfig(brokerURL, cfg)
			if err != nil {
				return nil, fmt.Errorf("amqp091: dial: %w", err)
			}
			return wrapConnection(c), nil
		}
	}
	return func(brokerURL string) (amqpConnection, error) {
		c, err := amqp.DialConfig(brokerURL, cfg)
		if err != nil {
			return nil, fmt.Errorf("amqp091: dial: %w", err)
		}
		return wrapConnection(c), nil
	}
}

// injectCredentials merges username/password into the broker URL. An
// explicitly configured (or rotated — see Session.ApplyCredentials)
// username/password ALWAYS wins over userinfo embedded in the URL:
// otherwise a credential rotation would report success while every
// redial silently kept authenticating with the old, embedded (and
// presumably soon-to-be-revoked) credentials. URL userinfo is used
// only when no explicit username is configured.
func injectCredentials(brokerURL, username, password string) string {
	if username == "" {
		return brokerURL
	}
	u, err := url.Parse(brokerURL)
	if err != nil {
		return brokerURL
	}
	u.User = url.UserPassword(username, password)
	return u.String()
}

// redactURL strips userinfo (credentials) from a URL for safe logging.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	if u.User != nil {
		u.User = url.User("REDACTED")
	}
	return u.String()
}

// brokerURLEmbedsUserinfo reports whether the broker URL carries
// userinfo (username[:password]). Explicit/rotated credentials override
// embedded userinfo on every dial (see injectCredentials), so when both
// are present the embedded values are dead config; the session Warns
// about the conflict (see Session.warnEmbeddedBrokerURLCredentials).
func brokerURLEmbedsUserinfo(brokerURL string) bool {
	u, err := url.Parse(brokerURL)
	if err != nil {
		return false
	}
	return u.User != nil && u.User.String() != ""
}
