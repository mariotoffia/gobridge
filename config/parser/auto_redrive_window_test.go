package parser

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

const autoRedriveWindowYAML = `
bridge:
  id: b
stores:
  dlq:
    type: memory
    auto_redrive_window: 2h
    options:
      acknowledge_volatile: true
`

func TestParse_CarriesDLQAutoRedriveWindow(t *testing.T) {
	cfg, err := Parse(strings.NewReader(autoRedriveWindowYAML), FormatYAML, passthroughRegistry("memory"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Stores.DLQ == nil || cfg.Stores.DLQ.AutoRedriveWindow != "2h" {
		t.Fatalf("stores.dlq = %+v, want auto_redrive_window 2h", cfg.Stores.DLQ)
	}
}

// A window written back by either wire format parses to the same value, so a
// config saved by the file, DynamoDB or CDK path keeps it.
func TestMarshal_RoundTripsDLQAutoRedriveWindow(t *testing.T) {
	cases := []struct {
		name    string
		format  Format
		marshal func(*ports.BridgeConfig) ([]byte, error)
	}{
		{"yaml", FormatYAML, MarshalYAML},
		{"json", FormatJSON, MarshalBridgeConfigJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := passthroughRegistry("memory")
			cfg, err := Parse(strings.NewReader(autoRedriveWindowYAML), FormatYAML, reg)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			out, err := tc.marshal(cfg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			again, err := Parse(bytes.NewReader(out), tc.format, reg)
			if err != nil {
				t.Fatalf("Parse again: %v\n%s", err, out)
			}
			if again.Stores.DLQ == nil || again.Stores.DLQ.AutoRedriveWindow != "2h" {
				t.Fatalf("round-tripped stores.dlq = %+v, want auto_redrive_window 2h\n%s", again.Stores.DLQ, out)
			}
		})
	}
}

func TestMarshal_OmitsAnUnsetAutoRedriveWindow(t *testing.T) {
	cfg, err := Parse(strings.NewReader("bridge:\n  id: b\nstores:\n  dlq:\n    type: memory\n"), FormatYAML, passthroughRegistry("memory"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := MarshalYAML(cfg)
	if err != nil {
		t.Fatalf("MarshalYAML: %v", err)
	}
	if bytes.Contains(out, []byte("auto_redrive_window")) {
		t.Fatalf("an unset window must not be written out:\n%s", out)
	}
}
