package main

import "testing"

// TestParseManagedSubscriptionBaselines_EmptyAndListedFilters verifies both flag forms.
func TestParseManagedSubscriptionBaselines_EmptyAndListedFilters(t *testing.T) {
	got, err := parseManagedSubscriptionBaselines([]string{
		"mqtt-conn",
		"legacy=orders/legacy/#,$share/group/orders/#",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v, ok := got["mqtt-conn"]; !ok || len(v) != 0 {
		t.Fatalf("mqtt-conn = %v, %v; want present and empty", v, ok)
	}
	if v := got["legacy"]; len(v) != 2 || v[0] != "orders/legacy/#" || v[1] != "$share/group/orders/#" {
		t.Fatalf("legacy = %v", v)
	}
}

// TestParseManagedSubscriptionBaselines_RejectsMalformedValues verifies invalid flag values fail.
func TestParseManagedSubscriptionBaselines_RejectsMalformedValues(t *testing.T) {
	for _, bad := range [][]string{
		{""},
		{"=a/#"},
		{"s=a/#,"},
		{"s", "s"},
	} {
		if _, err := parseManagedSubscriptionBaselines(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}
