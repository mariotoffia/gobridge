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
	// IsClosed reports whether the connection has been closed.
	IsClosed() bool
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

// dialFunc abstracts the AMQP dial operation for test-double injection.
type dialFunc func(url string) (amqpConnection, error)

// defaultDialFromOpts returns a dialFunc that honours TLS and heartbeat
// configuration. When TLS is enabled, it builds a *tls.Config and uses
// amqp091.DialTLS_Config; otherwise it uses amqp091.DialConfig with the
// configured heartbeat interval.
func defaultDialFromOpts(opts SessionOptions) dialFunc {
	cfg := amqp.Config{
		Heartbeat: opts.Heartbeat,
	}
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

// injectCredentials merges username/password into the broker URL if they
// are set and the URL does not already contain user-info.
func injectCredentials(brokerURL, username, password string) string {
	if username == "" {
		return brokerURL
	}
	u, err := url.Parse(brokerURL)
	if err != nil {
		return brokerURL
	}
	if u.User != nil && u.User.Username() != "" {
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
