package runtime

import (
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// matchFacts are the facts of a record dead-lettered for sensors/+/temp on
// session plant-a whose history is stored under id-1.
func matchFacts() map[string]string {
	return map[string]string{
		routing.ExtraInfoSessionID:       "plant-a",
		routing.ExtraInfoManagedIdentity: "id-1",
		routing.ExtraInfoSubscription:    "sensors/+/temp",
	}
}

func matchRecord(id string, mode routing.RedriveMode, code string, info map[string]string) routing.DLQEntry {
	return routing.NewDLQEntry(routing.DLQEntrySpec{
		ID:          id,
		Envelope:    *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-" + id}),
		RouteID:     "r1",
		ErrorCode:   code,
		FailedAt:    time.Unix(1_700_000_000, 0),
		RedriveMode: mode,
		ExtraInfo:   info,
	})
}

func TestAutoRedriveEventMatches(t *testing.T) {
	removed := string(shared.ErrCodeSubscriptionRemoved)
	with := func(key, value string) map[string]string {
		info := matchFacts()
		info[key] = value
		return info
	}
	without := func(key string) map[string]string {
		info := matchFacts()
		delete(info, key)
		return info
	}
	event := func(facts ...map[string]string) autoRedriveEvent {
		return autoRedriveEvent{trigger: triggerSubscriptionAdded, sessionID: "plant-a", facts: facts}
	}
	cases := []struct {
		name   string
		ev     autoRedriveEvent
		record routing.DLQEntry
		want   bool
	}{
		{"same facts", event(matchFacts()), matchRecord("a", routing.RedriveAuto, removed, matchFacts()), true},
		{"one of several fact sets", event(with(routing.ExtraInfoSubscription, "other/#"), matchFacts()),
			matchRecord("a", routing.RedriveAuto, removed, matchFacts()), true},
		{"manual record", event(matchFacts()), matchRecord("a", routing.RedriveManual, removed, matchFacts()), false},
		{"other record type", event(matchFacts()), matchRecord("a", routing.RedriveAuto, string(shared.ErrCodeUnavailable), matchFacts()), false},
		{"other trigger", autoRedriveEvent{trigger: "other", facts: []map[string]string{matchFacts()}},
			matchRecord("a", routing.RedriveAuto, removed, matchFacts()), false},
		{"other filter", event(matchFacts()), matchRecord("a", routing.RedriveAuto, removed, with(routing.ExtraInfoSubscription, "other/#")), false},
		{"other identity", event(matchFacts()), matchRecord("a", routing.RedriveAuto, removed, with(routing.ExtraInfoManagedIdentity, "id-2")), false},
		{"other session", event(matchFacts()), matchRecord("a", routing.RedriveAuto, removed, with(routing.ExtraInfoSessionID, "plant-b")), false},
		{"record missing a fact", event(matchFacts()), matchRecord("a", routing.RedriveAuto, removed, without(routing.ExtraInfoManagedIdentity)), false},
		{"record without facts", event(matchFacts()), matchRecord("a", routing.RedriveAuto, removed, nil), false},
		{"event without facts", event(), matchRecord("a", routing.RedriveAuto, removed, matchFacts()), false},
		// Two empty values are equal, but an empty value is no fact at all.
		{"empty subscription on both sides", event(with(routing.ExtraInfoSubscription, "")),
			matchRecord("a", routing.RedriveAuto, removed, with(routing.ExtraInfoSubscription, "")), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ev.matches(tc.record); got != tc.want {
				t.Fatalf("matches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFactsAgreeNeedsEveryKeyNonEmptyAndEqual(t *testing.T) {
	keys := []string{"a", "b"}
	cases := []struct {
		name string
		a, b map[string]string
		want bool
	}{
		{"equal", map[string]string{"a": "1", "b": "2"}, map[string]string{"a": "1", "b": "2", "c": "3"}, true},
		{"one differs", map[string]string{"a": "1", "b": "2"}, map[string]string{"a": "1", "b": "3"}, false},
		{"one missing", map[string]string{"a": "1", "b": "2"}, map[string]string{"a": "1"}, false},
		{"both empty", map[string]string{"a": "1", "b": ""}, map[string]string{"a": "1", "b": ""}, false},
		{"both nil", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := factsAgree(keys, tc.a, tc.b); got != tc.want {
				t.Fatalf("factsAgree(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// BenchmarkAutoRedriveMatch is the per-record cost of a pass: matching one
// event against 1 000 records, half of them for the added filter.
func BenchmarkAutoRedriveMatch(b *testing.B) {
	ev := autoRedriveEvent{trigger: triggerSubscriptionAdded, sessionID: "plant-a", facts: []map[string]string{matchFacts()}}
	other := maps.Clone(matchFacts())
	other[routing.ExtraInfoSubscription] = "sensors/+/humidity"
	records := make([]routing.DLQEntry, 1000)
	for i := range records {
		info := matchFacts()
		if i%2 == 1 {
			info = other
		}
		records[i] = matchRecord(fmt.Sprintf("rec-%04d", i), routing.RedriveAuto, string(shared.ErrCodeSubscriptionRemoved), info)
	}
	b.ReportAllocs()
	for b.Loop() {
		n := 0
		for _, e := range records {
			if ev.matches(e) {
				n++
			}
		}
		if n != len(records)/2 {
			b.Fatalf("matched %d records, want %d", n, len(records)/2)
		}
	}
}
