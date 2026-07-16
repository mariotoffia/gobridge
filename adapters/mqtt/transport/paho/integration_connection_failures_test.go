package paho_test

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestConnectionFailure_BeforeCONNACK(t *testing.T) {
	requireConnectionIntegration(t)
	brokerURL, serverDone := startMQTTPacketServer(t, func(conn net.Conn) error {
		if err := readMQTTPacket(conn); err != nil {
			return err
		}
		return nil
	})

	err := startFailureSession(t, brokerURL, nil)
	if err == nil {
		t.Fatal("Start succeeded after the peer closed before CONNACK")
	}
	if serverErr := wait.RequireReceive(t, serverDone, 2*time.Second); serverErr != nil {
		t.Fatalf("pre-CONNACK fake: %v", serverErr)
	}
}

func TestConnectionFailure_AfterCONNACK(t *testing.T) {
	requireConnectionIntegration(t)
	drop := make(chan struct{})
	brokerURL, serverDone := startMQTTPacketServer(t, func(conn net.Conn) error {
		if err := readMQTTPacket(conn); err != nil {
			return err
		}
		if _, err := conn.Write([]byte{0x20, 0x03, 0x00, 0x00, 0x00}); err != nil {
			return err
		}
		<-drop
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session := newConnectionSession(brokerURL)
	if err := session.Start(ctx); err != nil {
		t.Fatalf("Start before post-CONNACK drop: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	close(drop)
	if serverErr := wait.RequireReceive(t, serverDone, 2*time.Second); serverErr != nil {
		t.Fatalf("post-CONNACK fake: %v", serverErr)
	}
	wait.Until(t, 2*time.Second, "post-CONNACK disconnect reflected in health", func() bool {
		return !session.Health(context.Background()).Connected
	})
}

func TestConnectionFailure_TCPAndDNS(t *testing.T) {
	requireConnectionIntegration(t)
	t.Run("tcp refused", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve TCP address: %v", err)
		}
		address := listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatalf("close reserved TCP address: %v", err)
		}
		if err := startFailureSession(t, "tcp://"+address, nil); err == nil {
			t.Fatal("Start succeeded against a closed TCP address")
		}
	})
	t.Run("reserved invalid DNS name", func(t *testing.T) {
		// .invalid is reserved by RFC 2606. Only bounded failure is asserted;
		// resolver-specific error text and timing are deliberately not contracts.
		if err := startFailureSession(t, "tcp://task14-does-not-exist.invalid:1883", nil); err == nil {
			t.Fatal("Start succeeded for a reserved invalid DNS name")
		}
	})
}

func TestConnectionFailure_TLSTrustAndHandshake(t *testing.T) {
	requireConnectionIntegration(t)
	t.Run("untrusted certificate", func(t *testing.T) {
		server := httptest.NewUnstartedServer(nil)
		server.Config.ErrorLog = log.New(io.Discard, "", 0)
		server.StartTLS()
		t.Cleanup(server.Close)
		brokerURL := "tls://" + strings.TrimPrefix(server.URL, "https://")
		if err := startFailureSession(t, brokerURL, func(opts *paho.SessionOptions) {
			opts.TLS = &paho.TLSConfig{Enable: true}
		}); err == nil {
			t.Fatal("Start trusted a self-signed TLS endpoint without its CA")
		}
	})
	t.Run("malformed handshake", func(t *testing.T) {
		brokerURL, serverDone := startMQTTPacketServer(t, func(conn net.Conn) error {
			var clientHello [5]byte
			if _, err := io.ReadFull(conn, clientHello[:]); err != nil {
				return err
			}
			_, err := conn.Write([]byte("not-tls"))
			return err
		})
		err := startFailureSession(t, strings.Replace(brokerURL, "tcp://", "tls://", 1),
			func(opts *paho.SessionOptions) {
				opts.TLS = &paho.TLSConfig{Enable: true, InsecureSkipVerify: true}
			})
		if err == nil {
			t.Fatal("Start succeeded after a malformed TLS handshake")
		}
		if serverErr := wait.RequireReceive(t, serverDone, 2*time.Second); serverErr != nil {
			t.Fatalf("TLS handshake fake: %v", serverErr)
		}
	})
}

func TestConnectionFailure_CredentialsNotAuthorized(t *testing.T) {
	requireConnectionIntegration(t)
	brokerURL, serverDone := startMQTTPacketServer(t, func(conn net.Conn) error {
		if err := readMQTTPacket(conn); err != nil {
			return err
		}
		_, err := conn.Write([]byte{0x20, 0x03, 0x00, 0x87, 0x00})
		return err
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	notAuthorized := make(chan error, 1)
	session := paho.NewSession(paho.SessionOptions{
		BrokerURLs:                []string{brokerURL},
		ClientID:                  "task14-not-authorized",
		KeepAlive:                 2,
		ConnectTimeout:            750 * time.Millisecond,
		ReconnectDelay:            50 * time.Millisecond,
		ReconnectTimeout:          250 * time.Millisecond,
		CleanStart:                true,
		Username:                  "denied",
		Password:                  shared.NewSecret("denied"),
		AllowPlaintextCredentials: true,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	session.SetAuthFailureCallback(func(err error) {
		if !errors.Is(err, shared.ErrNotAuthorized) {
			return
		}
		select {
		case notAuthorized <- err:
		default:
		}
		cancel()
	})
	err := session.Start(ctx)
	if err == nil {
		t.Fatal("Start succeeded after a not-authorized CONNACK")
	}
	if authErr := wait.RequireReceive(t, notAuthorized, 2*time.Second); !errors.Is(authErr, shared.ErrNotAuthorized) {
		t.Fatalf("auth callback error = %v, want ErrNotAuthorized", authErr)
	}
	if serverErr := wait.RequireReceive(t, serverDone, 2*time.Second); serverErr != nil {
		t.Fatalf("not-authorized fake: %v", serverErr)
	}
}

func requireConnectionIntegration(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("local-network MQTT connection integration test")
	}
}

func newConnectionSession(brokerURL string) *paho.Session {
	return paho.NewSession(paho.SessionOptions{
		BrokerURLs:       []string{brokerURL},
		ClientID:         "task14-connection-failure",
		KeepAlive:        2,
		ConnectTimeout:   750 * time.Millisecond,
		ReconnectDelay:   50 * time.Millisecond,
		ReconnectTimeout: 250 * time.Millisecond,
		CleanStart:       true,
	}, connectivity.SessionEphemeral, nil)
}

func startFailureSession(
	t *testing.T,
	brokerURL string,
	configure func(*paho.SessionOptions),
) error {
	t.Helper()
	options := paho.SessionOptions{
		BrokerURLs:       []string{brokerURL},
		ClientID:         "task14-connection-failure",
		KeepAlive:        2,
		ConnectTimeout:   750 * time.Millisecond,
		ReconnectDelay:   50 * time.Millisecond,
		ReconnectTimeout: 250 * time.Millisecond,
		CleanStart:       true,
	}
	if configure != nil {
		configure(&options)
	}
	session := paho.NewSession(options, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return session.Start(ctx)
}

func startMQTTPacketServer(
	t *testing.T,
	handle func(net.Conn) error,
) (string, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		_ = listener.Close()
		defer func() { _ = conn.Close() }()
		done <- handle(conn)
	}()
	return "tcp://" + listener.Addr().String(), done
}

func readMQTTPacket(reader io.Reader) error {
	var fixed [1]byte
	if _, err := io.ReadFull(reader, fixed[:]); err != nil {
		return err
	}
	remaining := 0
	multiplier := 1
	for {
		var encoded [1]byte
		if _, err := io.ReadFull(reader, encoded[:]); err != nil {
			return err
		}
		remaining += int(encoded[0]&0x7f) * multiplier
		if encoded[0]&0x80 == 0 {
			break
		}
		multiplier *= 128
		if multiplier > 128*128*128 {
			return io.ErrUnexpectedEOF
		}
	}
	_, err := io.CopyN(io.Discard, reader, int64(remaining))
	return err
}
