package main

import (
	"os"
	"testing"
	"time"
)

// TestMain shortens the module-propagation budget for the whole package.
//
// awaitModuleResolution waits up to ten minutes for proxy.golang.org to publish
// a just-pushed tag. That is correct in a release, where the alternative is
// burning a version train on a cache that had not caught up yet, but a test
// asserting the failure path would otherwise sit through the entire budget and
// blow the package timeout.
//
// The retry behaviour itself is still exercised — only the pacing changes — so
// a test that expects a resolution failure still gets one, just quickly.
func TestMain(m *testing.M) {
	modulePropagationBudget = 100 * time.Millisecond
	modulePropagationPoll = 10 * time.Millisecond
	os.Exit(m.Run())
}
