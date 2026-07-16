package prodid_test

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/mariotoffia/gobridge/tests/testutil/prodid"
)

func TestAccountant_ReconcileClassifiesObservations(t *testing.T) {
	accountant, err := prodid.New([]string{"producer-5", "producer-3", "producer-1", "producer-4", "producer-2"}, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	accountant.ObserveOutput("producer-1", "envelope-1")
	accountant.ObserveOutput("producer-1", "envelope-1")
	accountant.ObserveDLQ("producer-2", "shared-envelope")
	accountant.ObserveDrop("producer-3", "envelope-3")
	accountant.ObserveOutput("producer-4", "shared-envelope")
	accountant.ObserveOutput("unexpected-2", "envelope-9")
	accountant.ObserveDrop("unexpected-1", "")

	got := accountant.Reconcile()
	want := prodid.Report{
		Missing:              []string{"producer-5"},
		Duplicates:           []prodid.Duplicate{{ProducerKey: "producer-1", Count: 2}},
		Reordered:            []string{},
		DLQ:                  []string{"producer-2"},
		IntentionallyDropped: []string{"producer-3", "unexpected-1"},
		Unexpected:           []string{"unexpected-1", "unexpected-2"},
		IdentityCollisions: []prodid.IdentityCollision{{
			EnvelopeID:   "shared-envelope",
			ProducerKeys: []string{"producer-2", "producer-4"},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Reconcile:\n got: %s\nwant: %s", got.String(), want.String())
	}
	if got.Exact() {
		t.Fatal("report with missing, duplicate, unexpected, and collision observations must not be exact")
	}
}

func TestAccountant_ReconcileOrdered(t *testing.T) {
	accountant, err := prodid.New([]string{"a", "b", "c", "d"}, true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	accountant.ObserveOutput("b", "eb")
	accountant.ObserveOutput("a", "ea")
	accountant.ObserveDLQ("c", "ec")
	accountant.ObserveOutput("d", "ed")

	got := accountant.Reconcile()
	if !reflect.DeepEqual(got.Reordered, []string{"a", "b"}) {
		t.Fatalf("Reordered = %v, want [a b]", got.Reordered)
	}
	if len(got.Missing) != 0 {
		t.Fatalf("Missing = %v, want none", got.Missing)
	}
}

func TestAccountant_ConcurrentObservations(t *testing.T) {
	const count = 1000
	expected := make([]string, count)
	for i := range count {
		expected[i] = fmt.Sprintf("producer-%04d", i)
	}
	accountant, err := prodid.New(expected, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			accountant.ObserveOutput(expected[i], fmt.Sprintf("envelope-%04d", i))
		}()
	}
	wg.Wait()

	report := accountant.Reconcile()
	if !report.Exact() {
		t.Fatalf("concurrent report is not exact: %s", report.String())
	}
}

func TestAccountant_NewRejectsInvalidExpectedKeys(t *testing.T) {
	tests := []struct {
		name     string
		expected []string
	}{
		{name: "empty", expected: []string{""}},
		{name: "duplicate", expected: []string{"same", "same"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := prodid.New(test.expected, false); err == nil {
				t.Fatal("New returned nil error")
			}
		})
	}
}
