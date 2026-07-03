package amqp10

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/Azure/go-amqp"
)

// amqpConn is the unexported façade over *amqp.Conn. It exposes only
// domain-typed or wrapper-typed operations so that consumers outside
// this ACL never reference the SDK and never need to import
// "github.com/Azure/go-amqp".
type amqpConn interface {
	// NewSession opens a new AMQP session, returned as a wrapper that
	// itself confines SDK access.
	NewSession(ctx context.Context) (*amqpSessionLink, error)
	// Close terminates the underlying connection.
	Close() error
	// Done returns a channel closed when the connection is no longer
	// usable. Used by the session's monitor loop.
	Done() <-chan struct{}
}

// sdkConn adapts a real *amqp.Conn to amqpConn so the rest of the
// package never touches the SDK type directly.
type sdkConn struct {
	raw *amqp.Conn
}

func wrapConn(c *amqp.Conn) amqpConn { return &sdkConn{raw: c} }

func (c *sdkConn) NewSession(ctx context.Context) (*amqpSessionLink, error) {
	s, err := c.raw.NewSession(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("amqp10: new session: %w", err)
	}
	return &amqpSessionLink{raw: s}, nil
}

func (c *sdkConn) Close() error {
	if err := c.raw.Close(); err != nil {
		return fmt.Errorf("amqp10: close: %w", err)
	}
	return nil
}
func (c *sdkConn) Done() <-chan struct{} { return c.raw.Done() }

// dialFunc abstracts the AMQP 1.0 dial operation for test-double
// injection. It carries SessionOptions + credentials as inputs, never
// SDK types, so non-ACL files can hold or invoke a dialFunc without
// importing the SDK.
type dialFunc func(ctx context.Context, opts SessionOptions, creds amqp10Credentials) (amqpConn, error)

// defaultDial wraps amqp.Dial behind the dialFunc signature. Connection
// options (container ID, SASL, TLS, frame size, idle timeout) are
// assembled here from SessionOptions so callers never see *amqp.ConnOptions.
func defaultDial(ctx context.Context, opts SessionOptions, creds amqp10Credentials) (amqpConn, error) {
	connOpts := &amqp.ConnOptions{
		IdleTimeout:  opts.IdleTimeout,
		MaxFrameSize: opts.MaxFrameSize,
	}
	if opts.ContainerID != "" {
		connOpts.ContainerID = opts.ContainerID
	}
	switch strings.ToLower(opts.SASLMechanism) {
	case saslMechanismExternal:
		// mTLS-style client-certificate auth: identity is established by
		// the TLS layer; the empty authorization identity lets the broker
		// derive it from the certificate.
		connOpts.SASLType = amqp.SASLTypeExternal("")
	case saslMechanismAnonymous:
		connOpts.SASLType = amqp.SASLTypeAnonymous()
	case saslMechanismPlain:
		connOpts.SASLType = amqp.SASLTypePlain(creds.Username, creds.Password)
	default: // "" — infer: PLAIN when credentials are present.
		if creds.Username != "" {
			connOpts.SASLType = amqp.SASLTypePlain(creds.Username, creds.Password)
		}
	}
	if opts.TLS != nil && opts.TLS.Enable {
		tlsCfg, err := BuildTLSConfig(opts.TLS)
		if err != nil {
			return nil, fmt.Errorf("amqp10: invalid TLS configuration: %w", err)
		}
		connOpts.TLSConfig = tlsCfg
	}

	conn, err := amqp.Dial(ctx, opts.Address, connOpts)
	if err != nil {
		return nil, fmt.Errorf("amqp10: dial: %w", err)
	}
	return wrapConn(conn), nil
}

// redactURL masks any userinfo (credentials) in a broker URL so it is
// safe for logging. Returns "<invalid-url>" for unparseable input,
// matching the amqp091 transport so cross-transport log assertions are
// stable.
func redactURL(addr string) string {
	u, err := url.Parse(addr)
	if err != nil {
		return "<invalid-url>"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	return u.String()
}
