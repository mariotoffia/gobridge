package runtime_test

import (
	"slices"
	"testing"
	"time"
)

// Clustered automatic redrive (ADR 0019): the hook fires only where the
// session reconciles, and the managed identity keeps one member from
// redriving records another member's session wrote. Both members run route r1
// on session plant-a over one shared DLQ store.

// A non-exclusive session runs on every member under its own identity. The
// records carry member A's, so member B's added subscription leaves them and
// member A's redrives each once.
func TestAutoRedriveClusterRedrivesOnlyOnTheMemberWhoseIdentityWroteTheRecords(t *testing.T) {
	store := newOrderedDLQStore()
	a := newAutoRedriveMember(t, store, "member-a", "id-a")
	b := newAutoRedriveMember(t, store, "member-b", "id-b")
	a.start(t)
	b.start(t)
	a.seed(t, "rec-1", 3*time.Hour)
	a.seed(t, "rec-2", 2*time.Hour)
	a.seed(t, "rec-3", time.Hour)

	b.sess.subscriptionAdded(t, autoRedriveFilter)
	b.waitPasses(t, 1)
	if got := b.sender.tried(); len(got) != 0 {
		t.Fatalf("member B sent %v for member A's records, want nothing", got)
	}
	if n := store.count(); n != 3 {
		t.Fatalf("DLQ records after member B's event = %d, want 3", n)
	}

	a.sess.subscriptionAdded(t, autoRedriveFilter)
	a.eventually(t, "member A redrove its records", func() bool { return store.count() == 0 })
	if got := a.sender.delivered(); !slices.Equal(got, []string{"rec-1", "rec-2", "rec-3"}) {
		t.Fatalf("member A delivered %v, want each record once", got)
	}
	if got := b.sender.tried(); len(got) != 0 {
		t.Fatalf("member B sent %v, want nothing", got)
	}
}

// An exclusive session names one identity on every member, but only the lease
// owner's session reconciles, so only the owner's hook fires: the owner
// redrives every record once and the standby nothing.
func TestAutoRedriveClusterRedrivesOnlyOnTheMemberWhoseSessionReconciles(t *testing.T) {
	store := newOrderedDLQStore()
	owner := newAutoRedriveMember(t, store, "member-a", "id-1")
	standby := newAutoRedriveMember(t, store, "member-b", "id-1")
	owner.start(t)
	standby.start(t)
	owner.seed(t, "rec-1", 2*time.Hour)
	owner.seed(t, "rec-2", time.Hour)

	owner.sess.subscriptionAdded(t, autoRedriveFilter)
	owner.eventually(t, "the owner redrove every record", func() bool { return store.count() == 0 })
	if got := owner.sender.delivered(); !slices.Equal(got, []string{"rec-1", "rec-2"}) {
		t.Fatalf("owner delivered %v, want each record once", got)
	}
	if got := standby.sender.tried(); len(got) != 0 {
		t.Fatalf("standby sent %v, want nothing", got)
	}
}
