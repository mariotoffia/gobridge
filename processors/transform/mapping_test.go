package transform

import (
	"testing"
)

func TestToBool_ZeroValues(t *testing.T) {
	tests := []struct {
		name   string
		input  any
		expect bool
	}{
		{"int zero", int(0), false},
		{"int nonzero", int(42), true},
		{"int64 zero", int64(0), false},
		{"int64 nonzero", int64(1), true},
		{"int32 zero", int32(0), false},
		{"int32 nonzero", int32(7), true},
		{"float64 zero", float64(0.0), false},
		{"float64 nonzero", float64(3.14), true},
		{"float32 zero", float32(0.0), false},
		{"float32 nonzero", float32(1.0), true},
		{"bool true", true, true},
		{"bool false", false, false},
		{"string true", "true", true},
		{"string false", "false", false},
		{"string 1", "1", true},
		{"string 0", "0", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := toBool(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.expect {
				t.Fatalf("toBool(%v [%T]): got %v, want %v", tt.input, tt.input, result, tt.expect)
			}
		})
	}
}

func TestToBool_UnsupportedType(t *testing.T) {
	_, err := toBool([]int{1, 2, 3})
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
}
