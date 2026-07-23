package dockerexec

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestWaitProbe_SucceedsAfterFailures(t *testing.T) {
	calls := 0
	err := WaitProbe("thing", 2*time.Second, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return errors.New("not yet")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WaitProbe returned error: %v", err)
	}
	if calls < 3 {
		t.Fatalf("probe called %d times, want >= 3", calls)
	}
}

func TestWaitProbe_TimesOutWithLastError(t *testing.T) {
	err := WaitProbe("thing", 50*time.Millisecond, time.Millisecond, func() error {
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "thing") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error should carry subject and last probe error, got: %v", err)
	}
}

func TestStabilize_FailsOnFlakyProbe(t *testing.T) {
	calls := 0
	err := Stabilize(func() error {
		calls++
		if calls == 2 {
			return errors.New("crash-loop")
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "crash-loop") {
		t.Fatalf("expected the flaky probe error, got: %v", err)
	}
}

func TestStabilize_AllProbesPass(t *testing.T) {
	calls := 0
	if err := Stabilize(func() error { calls++; return nil }); err != nil {
		t.Fatalf("Stabilize returned error: %v", err)
	}
	if calls != stabilizeAttempts {
		t.Fatalf("probe called %d times, want %d", calls, stabilizeAttempts)
	}
}

func TestWaitTCP_ListenerAccepts(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	port := l.Addr().(*net.TCPAddr).Port

	if err := WaitTCP(port, 2*time.Second); err != nil {
		t.Fatalf("WaitTCP against live listener: %v", err)
	}
}

func TestWaitTCP_TimesOutOnClosedPort(t *testing.T) {
	port, err := FreePort()
	if err != nil {
		t.Fatal(err)
	}
	err = WaitTCP(port, 250*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout against closed port")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("127.0.0.1:%d", port)) {
		t.Fatalf("error should name the address, got: %v", err)
	}
}
