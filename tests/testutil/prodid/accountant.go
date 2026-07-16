// Package prodid provides exact producer-key accounting for end-to-end tests.
//
// Producer keys identify the source events being reconciled. Envelope IDs are
// recorded independently so tests can detect bridge-identity collisions without
// treating a generated bridge identity as a producer oracle.
package prodid

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

type destination uint8

const (
	outputDestination destination = iota
	dlqDestination
	dropDestination
)

type observation struct {
	producerKey string
	envelopeID  string
	destination destination
}

// Duplicate reports the total terminal observations for one producer key.
type Duplicate struct {
	ProducerKey string `json:"producer_key"`
	Count       int    `json:"count"`
}

// IdentityCollision reports one envelope identity associated with multiple
// producer keys.
type IdentityCollision struct {
	EnvelopeID   string   `json:"envelope_id"`
	ProducerKeys []string `json:"producer_keys"`
}

// Report is a deterministic reconciliation snapshot. Every slice is sorted.
type Report struct {
	Missing              []string            `json:"missing"`
	Duplicates           []Duplicate         `json:"duplicates"`
	Reordered            []string            `json:"reordered"`
	DLQ                  []string            `json:"dlq"`
	IntentionallyDropped []string            `json:"intentionally_dropped"`
	Unexpected           []string            `json:"unexpected"`
	IdentityCollisions   []IdentityCollision `json:"identity_collisions"`
}

// Exact reports whether every expected producer key has exactly one terminal
// observation, no unexpected key or identity collision exists, and requested
// ordering was preserved. DLQ and intentional-drop observations are accounted
// terminal outcomes, so they do not by themselves make a report inexact.
func (r Report) Exact() bool {
	return len(r.Missing) == 0 &&
		len(r.Duplicates) == 0 &&
		len(r.Reordered) == 0 &&
		len(r.Unexpected) == 0 &&
		len(r.IdentityCollisions) == 0
}

// String returns the report as deterministic JSON.
func (r Report) String() string {
	encoded, err := json.Marshal(r)
	if err != nil {
		return `{"error":"unable to marshal producer accounting report"}`
	}
	return string(encoded)
}

// Accountant records terminal observations safely from concurrent collectors.
type Accountant struct {
	mu           sync.Mutex
	expected     []string
	expectedKeys map[string]struct{}
	ordered      bool
	observations []observation
}

// New constructs an accountant. Expected producer keys must be non-empty and
// unique. Their input order defines the requested order when ordered is true.
//
// ponytail: ordered accounting assumes observations are serialized by one
// caller; mutex acquisition order is not a producer-order oracle. Add explicit
// producer ordinals only if a later ordered proof has multiple collectors.
func New(expected []string, ordered bool) (*Accountant, error) {
	keys := make(map[string]struct{}, len(expected))
	for _, key := range expected {
		if key == "" {
			return nil, fmt.Errorf("prodid: expected producer key is empty")
		}
		if _, exists := keys[key]; exists {
			return nil, fmt.Errorf("prodid: duplicate expected producer key %q", key)
		}
		keys[key] = struct{}{}
	}
	return &Accountant{
		expected:     append([]string(nil), expected...),
		expectedKeys: keys,
		ordered:      ordered,
	}, nil
}

// ObserveOutput records a downstream output. producerKey and envelopeID remain
// separate inputs by design.
func (a *Accountant) ObserveOutput(producerKey, envelopeID string) {
	a.observe(producerKey, envelopeID, outputDestination)
}

// ObserveDLQ records a DLQ terminal outcome.
func (a *Accountant) ObserveDLQ(producerKey, envelopeID string) {
	a.observe(producerKey, envelopeID, dlqDestination)
}

// ObserveDrop records an intentional-drop terminal outcome.
func (a *Accountant) ObserveDrop(producerKey, envelopeID string) {
	a.observe(producerKey, envelopeID, dropDestination)
}

func (a *Accountant) observe(producerKey, envelopeID string, destination destination) {
	a.mu.Lock()
	a.observations = append(a.observations, observation{
		producerKey: producerKey,
		envelopeID:  envelopeID,
		destination: destination,
	})
	a.mu.Unlock()
}

// Reconcile returns a concurrency-safe deterministic snapshot.
func (a *Accountant) Reconcile() Report {
	a.mu.Lock()
	observations := append([]observation(nil), a.observations...)
	expected := append([]string(nil), a.expected...)
	ordered := a.ordered
	expectedKeys := make(map[string]struct{}, len(a.expectedKeys))
	for key := range a.expectedKeys {
		expectedKeys[key] = struct{}{}
	}
	a.mu.Unlock()

	report := emptyReport()
	counts := make(map[string]int, len(observations))
	outputCounts := make(map[string]int, len(observations))
	dlqKeys := make(map[string]struct{})
	dropKeys := make(map[string]struct{})
	unexpected := make(map[string]struct{})
	identityKeys := make(map[string]map[string]struct{})
	outputOrder := make([]string, 0, len(observations))

	for _, current := range observations {
		counts[current.producerKey]++
		if _, expectedKey := expectedKeys[current.producerKey]; !expectedKey {
			unexpected[current.producerKey] = struct{}{}
		}
		switch current.destination {
		case outputDestination:
			outputCounts[current.producerKey]++
			if outputCounts[current.producerKey] == 1 {
				outputOrder = append(outputOrder, current.producerKey)
			}
		case dlqDestination:
			dlqKeys[current.producerKey] = struct{}{}
		case dropDestination:
			dropKeys[current.producerKey] = struct{}{}
		}
		if current.envelopeID != "" {
			if identityKeys[current.envelopeID] == nil {
				identityKeys[current.envelopeID] = make(map[string]struct{})
			}
			identityKeys[current.envelopeID][current.producerKey] = struct{}{}
		}
	}

	for _, key := range expected {
		if counts[key] == 0 {
			report.Missing = append(report.Missing, key)
		}
	}
	for key, count := range counts {
		if count > 1 {
			report.Duplicates = append(report.Duplicates, Duplicate{ProducerKey: key, Count: count})
		}
	}
	report.DLQ = sortedKeys(dlqKeys)
	report.IntentionallyDropped = sortedKeys(dropKeys)
	report.Unexpected = sortedKeys(unexpected)
	report.IdentityCollisions = collisions(identityKeys)
	if ordered {
		report.Reordered = reorderedKeys(expected, outputOrder, outputCounts, expectedKeys)
	}

	sort.Strings(report.Missing)
	sort.Slice(report.Duplicates, func(i, j int) bool {
		return report.Duplicates[i].ProducerKey < report.Duplicates[j].ProducerKey
	})
	return report
}

func emptyReport() Report {
	return Report{
		Missing:              []string{},
		Duplicates:           []Duplicate{},
		Reordered:            []string{},
		DLQ:                  []string{},
		IntentionallyDropped: []string{},
		Unexpected:           []string{},
		IdentityCollisions:   []IdentityCollision{},
	}
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func collisions(identityKeys map[string]map[string]struct{}) []IdentityCollision {
	result := make([]IdentityCollision, 0)
	for identity, keys := range identityKeys {
		if len(keys) > 1 {
			result = append(result, IdentityCollision{
				EnvelopeID:   identity,
				ProducerKeys: sortedKeys(keys),
			})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].EnvelopeID < result[j].EnvelopeID
	})
	return result
}

func reorderedKeys(
	expected, outputOrder []string,
	outputCounts map[string]int,
	expectedKeys map[string]struct{},
) []string {
	expectedOutput := make([]string, 0, len(outputOrder))
	for _, key := range expected {
		if outputCounts[key] > 0 {
			expectedOutput = append(expectedOutput, key)
		}
	}
	observedOutput := make([]string, 0, len(expectedOutput))
	for _, key := range outputOrder {
		if _, ok := expectedKeys[key]; ok {
			observedOutput = append(observedOutput, key)
		}
	}
	reordered := make(map[string]struct{})
	for i := 0; i < len(expectedOutput) && i < len(observedOutput); i++ {
		if expectedOutput[i] != observedOutput[i] {
			reordered[expectedOutput[i]] = struct{}{}
			reordered[observedOutput[i]] = struct{}{}
		}
	}
	return sortedKeys(reordered)
}
