package memoryoutbox_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox"
	"github.com/mariotoffia/gobridge/ports/storetest"
)

// Validates the in-memory outbox store against the shared conformance suite.
func TestOutboxStoreConformance(t *testing.T) {
	store := memoryoutbox.NewStore()
	storetest.RunOutboxStoreTests(t, store)
}
