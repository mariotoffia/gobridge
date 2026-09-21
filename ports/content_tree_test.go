package ports_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/ports"
)

// The rule every content projection applies to a decoded document tree: an
// absent collection and an empty one are the same thing, a value is a value.

func TestWithoutEmptyCollections_NothingIsNothing(t *testing.T) {
	for name, value := range map[string]any{
		"nil":                 nil,
		"empty object":        map[string]any{},
		"empty array":         []any{},
		"object of empties":   map[string]any{"a": []any{}, "b": map[string]any{}, "c": nil},
		"nested empty object": map[string]any{"a": map[string]any{"b": map[string]any{}}},
	} {
		t.Run(name, func(t *testing.T) {
			got, keep := ports.WithoutEmptyCollections(value)
			assert.False(t, keep)
			assert.Nil(t, got)
		})
	}
}

func TestWithoutEmptyCollections_DropsObjectKeysThatCarryNothing(t *testing.T) {
	got, keep := ports.WithoutEmptyCollections(map[string]any{
		"broker_urls": []any{},
		"headers":     map[string]any{},
		"client_id":   "c",
		"keep_alive":  float64(30),
	})
	assert.True(t, keep)
	assert.Equal(t, map[string]any{"client_id": "c", "keep_alive": float64(30)}, got)
}

func TestWithoutEmptyCollections_ScalarsAreValuesNotAbsences(t *testing.T) {
	got, keep := ports.WithoutEmptyCollections(map[string]any{
		"name":  "",
		"count": float64(0),
		"flag":  false,
	})
	assert.True(t, keep)
	assert.Equal(t, map[string]any{"name": "", "count": float64(0), "flag": false}, got)
}

func TestWithoutEmptyCollections_KeepsArrayPositions(t *testing.T) {
	got, keep := ports.WithoutEmptyCollections([]any{map[string]any{}, "x", []any{}})
	assert.True(t, keep)
	assert.Equal(t, []any{nil, "x", nil}, got, "position is meaning inside an array, so an empty element stays as a placeholder")
}
