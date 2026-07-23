package wait

import (
	"testing"
	"time"
)

func TestPoll_TrueWithinDeadline(t *testing.T) {
	calls := 0
	ok := Poll(2*time.Second, func() bool {
		calls++
		return calls >= 3
	})
	if !ok {
		t.Fatal("Poll should report true once cond holds")
	}
}

func TestPoll_FalseAfterDeadline(t *testing.T) {
	start := time.Now()
	ok := Poll(50*time.Millisecond, func() bool { return false })
	if ok {
		t.Fatal("Poll must report false when cond never holds")
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("Poll returned before its deadline elapsed")
	}
}
