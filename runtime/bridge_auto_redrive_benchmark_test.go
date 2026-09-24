package runtime_test

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkAutoRedrivePass is one full automatic redrive: the added
// subscription redrives 500 matching records through route r1 into a sender
// that accepts everything, inject-then-delete each. The records are re-seeded,
// untimed, before every pass. The simple case, matching alone, is
// BenchmarkAutoRedriveMatch.
func BenchmarkAutoRedrivePass(b *testing.B) {
	const records = 500
	f := newAutoRedriveFixture(b)
	f.start(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		for r := range records {
			f.seed(b, fmt.Sprintf("pass-%d-rec-%03d", i, r), time.Duration(records-r)*time.Minute)
		}
		b.StartTimer()
		f.sess.subscriptionAdded(b, autoRedriveFilter)
		f.eventually(b, "every record redriven", func() bool { return f.store.count() == 0 })
	}
}
