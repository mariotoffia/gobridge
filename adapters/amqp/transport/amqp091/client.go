package amqp091

import (
	"net/url"

	amqp "github.com/rabbitmq/amqp091-go"
)

// amqpConnection is the subset of *amqp091.Connection used by Session.
// It enables test-double injection.
type amqpConnection interface {
	Channel() (*amqp.Channel, error)
	Close() error
	NotifyClose(receiver chan *amqp.Error) chan *amqp.Error
	IsClosed() bool
}

var _ amqpConnection = (*amqp.Connection)(nil)

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
			return amqp.DialConfig(brokerURL, cfg)
		}
	}
	return func(brokerURL string) (amqpConnection, error) {
		return amqp.DialConfig(brokerURL, cfg)
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

