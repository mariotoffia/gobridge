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

// defaultDialFromOpts returns a dialFunc that honours TLS configuration.
// When TLS is enabled, it builds a *tls.Config from opts.TLS and uses
// amqp091.DialTLS; otherwise it falls back to amqp091.Dial.
func defaultDialFromOpts(opts SessionOptions) dialFunc {
	if opts.TLS != nil && opts.TLS.Enable {
		return func(brokerURL string) (amqpConnection, error) {
			tlsCfg, err := BuildTLSConfig(opts.TLS)
			if err != nil {
				return nil, err
			}
			return amqp.DialTLS(brokerURL, tlsCfg)
		}
	}
	return func(brokerURL string) (amqpConnection, error) {
		return amqp.Dial(brokerURL)
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

