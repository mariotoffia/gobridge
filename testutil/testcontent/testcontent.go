// Package testcontent provides deterministic content-verification helpers
// for integration and long-running tests. Every test message is tagged with
// a unique TID (test-message-ID) in both a header and a JSON payload field.
// Assertion functions compare sent vs received sets by TID, detecting lost,
// duplicated, or corrupted messages.
package testcontent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

const (
	// HeaderTID is the header key used to tag every test message with a unique ID.
	HeaderTID = "x-bridge.test-msg-id"
	// PayloadTIDField is the JSON field embedded in payloads for transports
	// that strip or don't preserve headers.
	PayloadTIDField = "_tid"
)

// TestingT is the subset of *testing.T used by assertion helpers.
type TestingT interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// Expected records what was sent.
type Expected struct {
	TID     string
	Payload []byte
	Headers map[string]any
	Subject string
}

// Received records what arrived.
type Received struct {
	TID     string
	Payload []byte
	Headers map[string]any
	Subject string
}

var tidCounter atomic.Uint64

// Tag stamps env with a unique TID in both the header and (if the payload
// is a JSON object) a _tid field. Returns the generated TID and a
// snapshot of the tagged envelope as an Expected value.
func Tag(env *messaging.Envelope) (string, Expected) {
	tidCounter.Add(1)
	tid := fmt.Sprintf("tid-%d-%d", time.Now().UnixNano(), tidCounter.Load())

	env.SetHeader(HeaderTID, tid)

	env.Payload = injectPayloadTID(env.Payload, tid)

	return tid, Expected{
		TID:     tid,
		Payload: cpBytes(env.Payload),
		Headers: cpHeaders(env.Headers()),
		Subject: env.Subject(),
	}
}

// TagN stamps N envelopes and returns the Expected list in order.
func TagN(envs []*messaging.Envelope) []Expected {
	out := make([]Expected, len(envs))
	for i, env := range envs {
		_, out[i] = Tag(env)
	}
	return out
}

// ExtractTID reads the TID from an envelope, trying the header first,
// then falling back to the JSON _tid field. Returns "" if neither is present.
func ExtractTID(env *messaging.Envelope) string {
	if env.Headers() != nil {
		if v, ok := messaging.GetHeaderString(env.Headers(), HeaderTID); ok && v != "" {
			return v
		}
	}
	return extractPayloadTID(env.Payload)
}

// ReceivedFromEnvelopes converts a slice of messaging.Envelope pointers
// to a slice of Received, extracting TIDs automatically.
func ReceivedFromEnvelopes(envs []*messaging.Envelope) []Received {
	out := make([]Received, len(envs))
	for i, env := range envs {
		out[i] = Received{
			TID:     ExtractTID(env),
			Payload: cpBytes(env.Payload),
			Headers: cpHeaders(env.Headers()),
			Subject: env.Subject(),
		}
	}
	return out
}

// ReceivedFromBodies converts raw SQS-style string bodies to Received
// values, extracting TIDs from the JSON _tid field.
func ReceivedFromBodies(bodies []string) []Received {
	out := make([]Received, len(bodies))
	for i, b := range bodies {
		payload := []byte(b)
		out[i] = Received{
			TID:     extractPayloadTID(payload),
			Payload: payload,
		}
	}
	return out
}

// MatchOpt configures assertion behaviour.
type MatchOpt func(*matchCfg)

type matchCfg struct {
	headerKeys    []string // if non-nil, only these headers are compared
	ignoreHeaders map[string]bool
	payloadExact  bool
	jsonFields    []jsonFieldCheck
}

type jsonFieldCheck struct {
	path  string
	value string
}

// MatchPayloadExact makes AssertContentMatches compare payload bytes exactly.
func MatchPayloadExact() MatchOpt {
	return func(c *matchCfg) { c.payloadExact = true }
}

// MatchHeadersSubset limits header comparison to the given keys.
func MatchHeadersSubset(keys ...string) MatchOpt {
	return func(c *matchCfg) { c.headerKeys = keys }
}

// IgnoreHeaders excludes the given header keys from comparison.
func IgnoreHeaders(keys ...string) MatchOpt {
	return func(c *matchCfg) {
		if c.ignoreHeaders == nil {
			c.ignoreHeaders = make(map[string]bool)
		}
		for _, k := range keys {
			c.ignoreHeaders[k] = true
		}
	}
}

// MatchPayloadJSONField adds a check that a JSON field in the payload
// matches the given value.
func MatchPayloadJSONField(path, value string) MatchOpt {
	return func(c *matchCfg) {
		c.jsonFields = append(c.jsonFields, jsonFieldCheck{path, value})
	}
}

// AssertReceivedSet verifies set(sent.TID) == set(received.TID).
// Reports missing and extra TIDs.
func AssertReceivedSet(t TestingT, sent []Expected, received []Received) {
	t.Helper()

	sentSet := make(map[string]bool, len(sent))
	for _, s := range sent {
		sentSet[s.TID] = true
	}

	rxSet := make(map[string]bool, len(received))
	for _, r := range received {
		if r.TID == "" {
			continue
		}
		rxSet[r.TID] = true
	}

	var missing, extra []string
	for tid := range sentSet {
		if !rxSet[tid] {
			missing = append(missing, tid)
		}
	}
	for tid := range rxSet {
		if !sentSet[tid] {
			extra = append(extra, tid)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("AssertReceivedSet: %d sent TID(s) not received: %v", len(missing), missing)
	}
	if len(extra) > 0 {
		t.Errorf("AssertReceivedSet: %d unexpected TID(s) received: %v", len(extra), extra)
	}
}

// AssertContentMatches verifies set equality on TIDs and, for each
// matched TID, compares payload and/or headers according to opts.
func AssertContentMatches(t TestingT, sent []Expected, received []Received, opts ...MatchOpt) {
	t.Helper()

	cfg := &matchCfg{}
	for _, o := range opts {
		o(cfg)
	}

	sentMap := make(map[string]Expected, len(sent))
	for _, s := range sent {
		sentMap[s.TID] = s
	}

	rxMap := make(map[string]Received, len(received))
	for _, r := range received {
		if r.TID == "" {
			t.Errorf("AssertContentMatches: received entry with empty TID (payload=%q)", truncate(r.Payload, 80))
			continue
		}
		rxMap[r.TID] = r
	}

	// Check set equality.
	for tid := range sentMap {
		if _, ok := rxMap[tid]; !ok {
			t.Errorf("AssertContentMatches: sent TID %q not found in received", tid)
		}
	}
	for tid := range rxMap {
		if _, ok := sentMap[tid]; !ok {
			t.Errorf("AssertContentMatches: received TID %q was never sent", tid)
		}
	}

	// Per-TID comparison.
	for tid, exp := range sentMap {
		got, ok := rxMap[tid]
		if !ok {
			continue
		}

		if cfg.payloadExact {
			// Strip _tid from both before comparing to avoid false diff
			// when transport re-serialises JSON.
			expP := stripPayloadTID(exp.Payload)
			gotP := stripPayloadTID(got.Payload)
			if !bytes.Equal(expP, gotP) {
				t.Errorf("AssertContentMatches [%s]: payload mismatch\n  want: %s\n  got:  %s",
					tid, truncate(expP, 120), truncate(gotP, 120))
			}
		}

		for _, jf := range cfg.jsonFields {
			v := extractJSONField(got.Payload, jf.path)
			if v != jf.value {
				t.Errorf("AssertContentMatches [%s]: JSON field %q = %q, want %q",
					tid, jf.path, v, jf.value)
			}
		}

		if cfg.headerKeys != nil {
			for _, k := range cfg.headerKeys {
				if cfg.ignoreHeaders != nil && cfg.ignoreHeaders[k] {
					continue
				}
				expV := headerString(exp.Headers, k)
				gotV := headerString(got.Headers, k)
				if expV != gotV {
					t.Errorf("AssertContentMatches [%s]: header %q = %q, want %q",
						tid, k, gotV, expV)
				}
			}
		}
	}
}

// AssertNoDuplicates verifies each TID appears at most once in received.
func AssertNoDuplicates(t TestingT, received []Received) {
	t.Helper()

	counts := make(map[string]int, len(received))
	for _, r := range received {
		if r.TID != "" {
			counts[r.TID]++
		}
	}

	var dups []string
	for tid, n := range counts {
		if n > 1 {
			dups = append(dups, fmt.Sprintf("%s(x%d)", tid, n))
		}
	}
	sort.Strings(dups)
	if len(dups) > 0 {
		t.Errorf("AssertNoDuplicates: %d TID(s) received more than once: %v", len(dups), dups)
	}
}

// AssertOrdered verifies that the TIDs in received appear in the same
// order as in sent. Extra or missing TIDs are ignored — use
// AssertReceivedSet for completeness checks.
func AssertOrdered(t TestingT, sent []Expected, received []Received) {
	t.Helper()

	sentOrder := make([]string, 0, len(sent))
	for _, s := range sent {
		sentOrder = append(sentOrder, s.TID)
	}

	rxOrder := make([]string, 0, len(received))
	for _, r := range received {
		if r.TID != "" {
			rxOrder = append(rxOrder, r.TID)
		}
	}

	si, ri := 0, 0
	for si < len(sentOrder) && ri < len(rxOrder) {
		if sentOrder[si] == rxOrder[ri] {
			si++
			ri++
		} else {
			ri++
		}
	}
	if si < len(sentOrder) {
		t.Errorf("AssertOrdered: received TIDs are not in sent order; first missed TID at sent index %d: %s",
			si, sentOrder[si])
	}
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func injectPayloadTID(payload []byte, tid string) []byte {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || payload[0] != '{' {
		return payload
	}
	tidJSON := fmt.Sprintf("%q:%q", PayloadTIDField, tid)
	if len(payload) == 2 { // "{}"
		return []byte("{" + tidJSON + "}")
	}
	return append([]byte("{"+tidJSON+","), payload[1:]...)
}

func extractPayloadTID(payload []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil {
		return ""
	}
	raw, ok := m[PayloadTIDField]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func stripPayloadTID(payload []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil {
		return payload
	}
	delete(m, PayloadTIDField)
	out, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return out
}

func extractJSONField(payload []byte, path string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil {
		return ""
	}
	raw, ok := m[path]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.Trim(string(raw), " \t\n\"")
}

func headerString(h map[string]any, key string) string {
	if h == nil {
		return ""
	}
	v, ok := h[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}

func cpBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

func cpHeaders(h map[string]any) map[string]any {
	if h == nil {
		return nil
	}
	c := make(map[string]any, len(h))
	for k, v := range h {
		c[k] = v
	}
	return c
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
