package paho_test

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

const task14DNSChildEnv = "GOBRIDGE_TASK14_DNS_CHILD"

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
	if os.Getenv(task14DNSChildEnv) == "1" {
		testHermeticDNSFailure(t)
		return
	}
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
	t.Run("hermetic DNS name error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0],
			"-test.run=^TestConnectionFailure_TCPAndDNS$",
			"-test.count=1",
		)
		command.Env = dnsChildEnvironment()
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("DNS failure subprocess: %v\n%s", err, output)
		}
	})
}

func dnsChildEnvironment() []string {
	environment := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key != task14DNSChildEnv && key != "all_proxy" {
			environment = append(environment, entry)
		}
	}
	return append(environment, task14DNSChildEnv+"=1", "all_proxy=")
}

func testHermeticDNSFailure(t *testing.T) {
	const hostname = "task14-dns.test"
	server := startLoopbackDNSServer(t)
	originalResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			dialer := net.Dialer{}
			return dialer.DialContext(ctx, network, server.address)
		},
	}
	t.Cleanup(func() { net.DefaultResolver = originalResolver })

	if err := startFailureSession(t, "tcp://"+hostname+":1883", nil); err == nil {
		t.Fatal("Start succeeded after the loopback DNS server returned NXDOMAIN")
	}
	query := wait.RequireReceive(t, server.queries, 2*time.Second)
	if query.name != hostname+"." {
		t.Fatalf("DNS query name = %q, want %q", query.name, hostname+".")
	}
	if query.recordType != dnsmessage.TypeA && query.recordType != dnsmessage.TypeAAAA {
		t.Fatalf("DNS query type = %v, want A or AAAA", query.recordType)
	}
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

type observedDNSQuery struct {
	name       string
	recordType dnsmessage.Type
}

type loopbackDNSServer struct {
	address string
	queries chan observedDNSQuery
	done    chan struct{}
}

func startLoopbackDNSServer(t *testing.T) *loopbackDNSServer {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen for loopback DNS: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})
	server := &loopbackDNSServer{
		address: conn.LocalAddr().String(),
		queries: make(chan observedDNSQuery, 8),
		done:    done,
	}
	go func() {
		defer close(server.done)
		buffer := make([]byte, 1232)
		for {
			count, client, readErr := conn.ReadFromUDP(buffer)
			if readErr != nil {
				return
			}
			var parser dnsmessage.Parser
			header, parseErr := parser.Start(buffer[:count])
			if parseErr != nil {
				continue
			}
			question, questionErr := parser.Question()
			if questionErr != nil {
				continue
			}
			builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{
				ID:                 header.ID,
				Response:           true,
				Authoritative:      true,
				RecursionDesired:   header.RecursionDesired,
				RecursionAvailable: true,
				RCode:              dnsmessage.RCodeNameError,
			})
			if buildErr := builder.StartQuestions(); buildErr != nil {
				continue
			}
			if buildErr := builder.Question(question); buildErr != nil {
				continue
			}
			response, buildErr := builder.Finish()
			if buildErr != nil {
				continue
			}
			if _, writeErr := conn.WriteToUDP(response, client); writeErr != nil {
				continue
			}
			select {
			case server.queries <- observedDNSQuery{
				name: question.Name.String(), recordType: question.Type,
			}:
			default:
			}
		}
	}()
	return server
}
