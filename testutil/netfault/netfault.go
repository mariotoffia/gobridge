// Package netfault provides a bounded TCP fault-injection proxy for tests that
// need a network to misbehave in a specific, repeatable way.
//
// Reconnect, acknowledgement and shutdown behaviour under partial network
// failure cannot be proved by stopping a container: that is one failure mode
// (the peer goes away cleanly) out of several, and it is the mildest. The ones
// that hurt in production are the ones where the socket stays up:
//
//   - a PARTITION, where the peer becomes unreachable and every live
//     connection dies — [Proxy.Cut];
//   - a HALF-OPEN connection, where the socket is established and writable and
//     nothing is ever delivered again — [Proxy.Blackhole]. This is what packet
//     loss looks like to an application once retransmission has given up, and
//     it is the failure a keep-alive exists to detect;
//   - LATENCY, where everything still arrives, late enough to expose a timeout
//     that was tuned against a loopback — [Proxy.SetLatency];
//   - an endpoint that gives a NEW client nothing while its existing
//     connections keep working — [Proxy.RefuseNew], which is what a rolling
//     broker restart or a removed endpoint looks like to a client holding
//     multiple URLs.
//
// A refusing proxy keeps its listener bound and resets each new connection the
// instant it is accepted, rather than unbinding the port. Unbinding would race
// another process for the port before [Proxy.Heal] could reclaim it, which
// would make the fault injector itself the flaky part; and to the client both
// are the same thing — a transport that will not carry a session.
//
// Every fault is reversible ([Proxy.Heal]) and bounded by the test's own
// lifetime: the listener and every connection it carries are torn down through
// t.Cleanup.
//
// Genuine per-segment packet loss is deliberately NOT modelled. A TCP proxy
// that dropped application bytes would corrupt the stream rather than lose a
// segment, which is a failure no real network produces and no code should be
// asked to survive. Blackhole is the honest application-visible form.
package netfault

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// Proxy forwards TCP connections to a target and can be told to misbehave.
// Every method is safe to call from any goroutine at any time.
type Proxy struct {
	target   string
	listener net.Listener

	mu        sync.Mutex
	refusing  bool
	blackhole bool
	latency   time.Duration
	live      map[net.Conn]struct{}
	closed    bool

	accepted int64
}

// Start begins forwarding to target on a fresh loopback port. The proxy and
// everything it carries are torn down when the test ends.
func Start(t testing.TB, target string) *Proxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("netfault: listen: %v", err)
	}
	p := &Proxy{
		target:   target,
		listener: listener,
		live:     map[net.Conn]struct{}{},
	}
	go p.serve()
	t.Cleanup(p.Close)
	return p
}

// Addr is the proxy's host:port. Point the client under test at this instead
// of at the real endpoint.
func (p *Proxy) Addr() string { return p.listener.Addr().String() }

// URL renders the proxy address with a scheme, e.g. URL("tcp") for a broker URL.
func (p *Proxy) URL(scheme string) string { return fmt.Sprintf("%s://%s", scheme, p.Addr()) }

// Target is the endpoint this proxy forwards to.
func (p *Proxy) Target() string { return p.target }

// Cut simulates a network partition: every live connection is torn down and
// new ones are reset on accept until [Proxy.Heal].
func (p *Proxy) Cut() {
	p.mu.Lock()
	p.refusing = true
	conns := make([]net.Conn, 0, len(p.live))
	for conn := range p.live {
		conns = append(conns, conn)
	}
	p.live = map[net.Conn]struct{}{}
	p.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}
}

// RefuseNew resets every new connection while leaving live ones intact — an
// endpoint that has gone away for anyone not already talking to it.
func (p *Proxy) RefuseNew() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refusing = true
}

// Blackhole stops delivering bytes in both directions on every connection,
// live and future, without closing anything: the half-open case.
//
// Bytes read while blackholed are DISCARDED, not buffered — reading them is
// what stops the sender's window from filling and turning a stall into a
// visible error, which is the whole point. A connection that was blackholed has
// therefore lost part of its stream and can never be resumed, so [Proxy.Heal]
// closes it rather than returning a corrupt connection to service.
func (p *Proxy) Blackhole() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blackhole = true
}

// SetLatency delays every forwarded chunk by d. Zero restores immediate
// forwarding.
func (p *Proxy) SetLatency(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.latency = d
}

// Heal clears every injected fault, and closes any connection that was
// blackholed (see [Proxy.Blackhole]). Connections cut earlier stay cut — a
// healed network does not resurrect a closed socket — but new ones succeed.
func (p *Proxy) Heal() {
	p.mu.Lock()
	p.refusing = false
	p.latency = 0
	poisoned := p.blackhole
	p.blackhole = false
	var conns []net.Conn
	if poisoned {
		for conn := range p.live {
			conns = append(conns, conn)
		}
		p.live = map[net.Conn]struct{}{}
	}
	p.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}
}

// Accepted reports how many connections the proxy has accepted since it
// started, so a test can assert that a client actually reconnected rather than
// inferring it from message flow.
func (p *Proxy) Accepted() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accepted
}

// Close stops the proxy and every connection it carries. Safe to call twice.
func (p *Proxy) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	conns := make([]net.Conn, 0, len(p.live))
	for conn := range p.live {
		conns = append(conns, conn)
	}
	p.live = map[net.Conn]struct{}{}
	p.mu.Unlock()

	_ = p.listener.Close()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (p *Proxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		// Deciding to serve and registering the connection happen under ONE
		// lock: a Cut landing between the two would otherwise leave a
		// connection alive that Cut believed it had torn down.
		p.mu.Lock()
		serve := !p.refusing && !p.closed
		if serve {
			p.accepted++
			p.live[client] = struct{}{}
		}
		p.mu.Unlock()

		if !serve {
			_ = client.Close()
			continue
		}
		go p.forward(client)
	}
}

func (p *Proxy) forward(client net.Conn) {
	upstream, err := net.DialTimeout("tcp", p.target, 10*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}

	p.track(upstream)

	done := make(chan struct{}, 2)
	go p.pipe(upstream, client, done)
	go p.pipe(client, upstream, done)

	<-done
	_ = client.Close()
	_ = upstream.Close()
	p.untrack(client)
	p.untrack(upstream)
}

// pipe copies src into dst, applying the currently configured fault to every
// chunk. The fault is read per chunk rather than captured once, so a fault
// injected mid-stream takes effect on the connections already open.
func (p *Proxy) pipe(dst, src net.Conn, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()

	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			blackhole, latency := p.faults()
			if blackhole {
				// Keep reading so the sender's window does not fill and turn a
				// stall into a visible error: a half-open connection accepts
				// bytes and never delivers them.
				continue
			}
			if latency > 0 && !p.delay(latency) {
				return
			}
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// delay waits out the injected latency, returning false if the proxy closed
// first so a shutting-down test is not held by its own fault injection.
func (p *Proxy) delay(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	<-timer.C
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.closed
}

func (p *Proxy) faults() (bool, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.blackhole, p.latency
}

func (p *Proxy) track(conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = conn.Close()
		return
	}
	p.live[conn] = struct{}{}
}

func (p *Proxy) untrack(conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.live, conn)
}
