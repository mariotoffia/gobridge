package mqttlocal

import (
	"fmt"
	"runtime"
	"testing"
)

// The network addresses are what a client in another container dials, so an
// address for a listener or network the broker does not have would send that
// client to a name or port that never answers. And the network is fixed when
// the container is created: a restart that accepted another network would
// report a broker on a network it never joined. Both pinned without Docker.
//
// Category: unit (TESTS.md §1).

func TestBrokerInstance_NetworkURLsNeedWithNetwork(t *testing.T) {
	const name = "gobridge-mqttinst-1"
	for _, tc := range []struct {
		desc        string
		opts        []Option
		url, tlsURL string
	}{
		{desc: "no network", opts: []Option{WithTLS()}},
		{desc: "network without TLS", opts: []Option{WithNetwork("net")},
			url: "tcp://" + name + ":1883"},
		{desc: "network with TLS", opts: []Option{WithNetwork("net"), WithTLS()},
			url: "tcp://" + name + ":1883", tlsURL: "ssl://" + name + ":8883"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			c := defaultConfig()
			for _, o := range tc.opts {
				o(&c)
			}
			b := &BrokerInstance{cfg: c, name: name}
			if got := b.NetworkURL(); got != tc.url {
				t.Errorf("NetworkURL() = %q, want %q", got, tc.url)
			}
			if got := b.NetworkTLSURL(); got != tc.tlsURL {
				t.Errorf("NetworkTLSURL() = %q, want %q", got, tc.tlsURL)
			}
		})
	}
}

func TestBrokerInstance_RestartWithRefusesAnotherNetwork(t *testing.T) {
	c := defaultConfig()
	WithNetwork("created-on")(&c)
	recorder := &fatalRecorder{TB: t}
	// stopped, so a RestartWith that wrongly accepted the option would reach
	// the config rewrite and fail there with a different message, never docker.
	b := &BrokerInstance{t: recorder, cfg: c, name: "gobridge-mqttinst-1", stopped: true}

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.RestartWith(WithNetwork("other"))
	}()
	<-done

	const want = "mqttlocal.BrokerInstance.RestartWith: listeners, credentials, TLS, " +
		"the ACL, persistence and the Docker network are fixed at NewBrokerInstance"
	if recorder.message != want {
		t.Fatalf("RestartWith(WithNetwork(other)) fatal message = %q, want %q", recorder.message, want)
	}
	if b.cfg.network != "created-on" {
		t.Fatalf("the refused restart changed the recorded network to %q", b.cfg.network)
	}
}

// fatalRecorder stands in for the test a BrokerInstance fails: it records the
// fatal message and ends the calling goroutine, as testing.T.Fatalf does.
type fatalRecorder struct {
	testing.TB
	message string
}

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}
