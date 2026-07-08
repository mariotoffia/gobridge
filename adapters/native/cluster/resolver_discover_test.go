package cluster

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
)

// ipnet is a small helper building an interface address for the addrs seam.
func ipnet(ip string, ones int) net.Addr {
	return &net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(ones, 32)}
}

// warnLogger returns a logger that captures WARN+ records into buf.
func warnLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// TestDiscoverHost_MultipleCandidates_WarnsAndPicksFirst covers finding F8: on a
// multi-homed host the resolver must pick the first non-loopback IPv4
// deterministically AND emit an ambiguity WARN so operators can pin the address.
func TestDiscoverHost_MultipleCandidates_WarnsAndPicksFirst(t *testing.T) {
	var buf bytes.Buffer
	r := NewNativeEndpointResolver(WithLogger(warnLogger(&buf)))
	r.addrs = func() ([]net.Addr, error) {
		return []net.Addr{
			ipnet("127.0.0.1", 8),   // loopback, ignored
			ipnet("10.0.0.5", 24),   // candidate 1 (first wins)
			ipnet("172.17.0.1", 16), // candidate 2 (docker0)
		}, nil
	}

	host, err := r.discoverHost(context.Background())
	if err != nil {
		t.Fatalf("discoverHost: %v", err)
	}
	if host != "10.0.0.5" {
		t.Errorf("host = %q, want deterministic first candidate 10.0.0.5", host)
	}
	if logs := buf.String(); !strings.Contains(logs, "multiple non-loopback IPv4") {
		t.Errorf("expected ambiguity WARN, got logs: %q", logs)
	}
}

// TestDiscoverHost_SingleCandidate_NoWarn confirms the common single-NIC case
// picks the address with no spurious warning.
func TestDiscoverHost_SingleCandidate_NoWarn(t *testing.T) {
	var buf bytes.Buffer
	r := NewNativeEndpointResolver(WithLogger(warnLogger(&buf)))
	r.addrs = func() ([]net.Addr, error) {
		return []net.Addr{
			ipnet("127.0.0.1", 8),
			ipnet("10.0.0.5", 24),
		}, nil
	}

	host, err := r.discoverHost(context.Background())
	if err != nil {
		t.Fatalf("discoverHost: %v", err)
	}
	if host != "10.0.0.5" {
		t.Errorf("host = %q, want 10.0.0.5", host)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no WARN for a single candidate, got: %q", buf.String())
	}
}

// TestDiscoverHost_NoCandidates_Error confirms an all-loopback host errors.
func TestDiscoverHost_NoCandidates_Error(t *testing.T) {
	r := NewNativeEndpointResolver()
	r.addrs = func() ([]net.Addr, error) {
		return []net.Addr{ipnet("127.0.0.1", 8)}, nil
	}
	if _, err := r.discoverHost(context.Background()); err == nil {
		t.Fatal("expected error when no non-loopback IPv4 candidate exists")
	}
}

// TestDiscoverHost_MultipleCandidates_NoLoggerNoPanic confirms the WARN path is
// safely skipped when no logger is configured.
func TestDiscoverHost_MultipleCandidates_NoLoggerNoPanic(t *testing.T) {
	r := NewNativeEndpointResolver()
	r.addrs = func() ([]net.Addr, error) {
		return []net.Addr{ipnet("10.0.0.5", 24), ipnet("172.17.0.1", 16)}, nil
	}
	host, err := r.discoverHost(context.Background())
	if err != nil {
		t.Fatalf("discoverHost: %v", err)
	}
	if host != "10.0.0.5" {
		t.Errorf("host = %q, want 10.0.0.5", host)
	}
}
