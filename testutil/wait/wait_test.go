package wait

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockT captures Fatalf calls without actually failing.
type mockT struct {
	mu       sync.Mutex
	fatals   []string
	deadline time.Time
	hasDD    bool
}

func (m *mockT) Helper() {}

func (m *mockT) Fatalf(format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fatals = append(m.fatals, format)
}

func (m *mockT) Deadline() (time.Time, bool) {
	return m.deadline, m.hasDD
}

func (m *mockT) fatalCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.fatals)
}

func TestRequireReceive_Success(t *testing.T) {
	ch := make(chan int, 1)
	ch <- 42

	got := RequireReceive[int](t, ch, time.Second)
	if got != 42 {
		t.Fatalf("expected 42, got %d", got)
	}
}

func TestRequireReceive_Timeout(t *testing.T) {
	ch := make(chan int)
	mt := &mockT{}

	RequireReceive[int](mt, ch, 10*time.Millisecond)

	if mt.fatalCount() == 0 {
		t.Fatal("expected Fatalf to be called on timeout")
	}
}

func TestRequireClosed_Success(t *testing.T) {
	ch := make(chan int)
	close(ch)

	RequireClosed[int](t, ch, time.Second)
}

func TestRequireClosed_DrainsBeforeClose(t *testing.T) {
	ch := make(chan int, 2)
	ch <- 1
	ch <- 2
	close(ch)

	RequireClosed[int](t, ch, time.Second)
}

func TestSilent_Success(t *testing.T) {
	ch := make(chan int)
	Silent[int](t, ch, 20*time.Millisecond)
}

func TestSilent_FailsOnReceive(t *testing.T) {
	ch := make(chan int, 1)
	ch <- 1
	mt := &mockT{}

	Silent[int](mt, ch, 50*time.Millisecond)

	if mt.fatalCount() == 0 {
		t.Fatal("expected Fatalf when channel had a value")
	}
}

func TestUntil_Success(t *testing.T) {
	var counter atomic.Int32

	go func() {
		time.Sleep(10 * time.Millisecond)
		counter.Store(5)
	}()

	Until(t, time.Second, "counter reaches 5", func() bool {
		return counter.Load() == 5
	})
}

func TestUntil_Timeout(t *testing.T) {
	mt := &mockT{}

	Until(mt, 20*time.Millisecond, "never true", func() bool {
		return false
	})

	if mt.fatalCount() == 0 {
		t.Fatal("expected Fatalf on timeout")
	}
}

func TestStableFor_Success(t *testing.T) {
	val := atomic.Int32{}
	val.Store(7)

	got := StableFor[int32](t, func() int32 {
		return val.Load()
	}, 30*time.Millisecond, time.Second)

	if got != 7 {
		t.Fatalf("expected 7, got %d", got)
	}
}

func TestStableFor_DetectsChange(t *testing.T) {
	var val atomic.Int32

	go func() {
		for i := 0; i < 100; i++ {
			time.Sleep(2 * time.Millisecond)
			val.Add(1)
		}
	}()

	mt := &mockT{}
	StableFor[int32](mt, func() int32 {
		return val.Load()
	}, 50*time.Millisecond, 30*time.Millisecond)

	if mt.fatalCount() == 0 {
		t.Fatal("expected Fatalf when value keeps changing")
	}
}
