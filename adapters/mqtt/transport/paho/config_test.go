package paho

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/ports"
)

// verifies SessionOptionsFromMap applies defaults when the input map is nil.
func TestSessionOptionsFromMap_Defaults(t *testing.T) {
	opts, err := SessionOptionsFromMap(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.KeepAlive != 30 {
		t.Errorf("KeepAlive = %d, want 30", opts.KeepAlive)
	}
	if opts.ConnectTimeout != 30*time.Second {
		t.Errorf("ConnectTimeout = %v, want 30s", opts.ConnectTimeout)
	}
	if !opts.CleanStart {
		t.Error("CleanStart should default to true")
	}
}

// verifies SessionOptionsFromMap reads broker_urls and client_id.
func TestSessionOptionsFromMap_BrokerURLs(t *testing.T) {
	m := map[string]any{
		"broker_urls": []string{"tcp://a:1883", "tcp://b:1883"},
		"client_id":   "test-client",
	}
	opts, err := SessionOptionsFromMap(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(opts.BrokerURLs) != 2 {
		t.Fatalf("BrokerURLs len = %d, want 2", len(opts.BrokerURLs))
	}
	if opts.ClientID != "test-client" {
		t.Errorf("ClientID = %q, want %q", opts.ClientID, "test-client")
	}
}

// verifies SessionOptionsFromMap accepts a single broker_url string.
func TestSessionOptionsFromMap_SingleBrokerURL(t *testing.T) {
	m := map[string]any{
		"broker_url": "tcp://single:1883",
	}
	opts, err := SessionOptionsFromMap(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(opts.BrokerURLs) != 1 || opts.BrokerURLs[0] != "tcp://single:1883" {
		t.Errorf("BrokerURLs = %v, want [tcp://single:1883]", opts.BrokerURLs)
	}
}

// verifies SessionOptionsFromMap maps username and password.
func TestSessionOptionsFromMap_Auth(t *testing.T) {
	m := map[string]any{
		"username": "user",
		"password": "pass",
	}
	opts, err := SessionOptionsFromMap(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if opts.Username != "user" {
		t.Errorf("Username = %q, want %q", opts.Username, "user")
	}
	if opts.Password != "pass" {
		t.Errorf("Password = %q, want %q", opts.Password, "pass")
	}
}

// verifies SessionOptionsFromMap builds TLS options from a nested tls map.
func TestSessionOptionsFromMap_TLSFromMap(t *testing.T) {
	m := map[string]any{
		"tls": map[string]any{
			"enable":               true,
			"insecure_skip_verify": true,
		},
	}
	opts, err := SessionOptionsFromMap(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if opts.TLS == nil {
		t.Fatal("TLS should be set")
	}
	if !opts.TLS.Enable {
		t.Error("TLS.Enable should be true")
	}
	if !opts.TLS.InsecureSkipVerify {
		t.Error("TLS.InsecureSkipVerify should be true")
	}
}

// verifies SessionOptionsFromMap maps session_expiry_interval and clean_start.
func TestSessionOptionsFromMap_SessionExpiry(t *testing.T) {
	m := map[string]any{
		"session_expiry_interval": 3600,
		"clean_start":             false,
	}
	opts, err := SessionOptionsFromMap(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if opts.SessionExpiryInterval != 3600 {
		t.Errorf("SessionExpiryInterval = %d, want 3600", opts.SessionExpiryInterval)
	}
	if opts.CleanStart {
		t.Error("CleanStart should be false")
	}
}

// verifies SessionOptionsFromMap rejects keep_alive outside uint16 range.
func TestSessionOptionsFromMap_InvalidKeepAlive(t *testing.T) {
	tests := []struct {
		name string
		val  int
	}{
		{"negative", -1},
		{"too_large", 70000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{"keep_alive": tc.val}
			_, err := SessionOptionsFromMap(m)
			if err == nil {
				t.Errorf("expected error for keep_alive=%d", tc.val)
			}
		})
	}
}

// verifies SenderOptionsFromMap applies defaults when the input map is nil.
func TestSenderOptionsFromMap_Defaults(t *testing.T) {
	opts, err := SenderOptionsFromMap(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.QoS != 1 {
		t.Errorf("QoS = %d, want 1", opts.QoS)
	}
	if opts.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", opts.Timeout)
	}
}

// verifies SenderOptionsFromMap maps default_topic, qos, retain, and timeout.
func TestSenderOptionsFromMap_AllFields(t *testing.T) {
	m := map[string]any{
		"default_topic": "my/topic",
		"qos":           2,
		"retain":        true,
		"timeout":       10 * time.Second,
	}
	opts, err := SenderOptionsFromMap(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if opts.DefaultTopic != "my/topic" {
		t.Errorf("DefaultTopic = %q, want %q", opts.DefaultTopic, "my/topic")
	}
	if opts.QoS != 2 {
		t.Errorf("QoS = %d, want 2", opts.QoS)
	}
	if !opts.Retain {
		t.Error("Retain should be true")
	}
	if opts.Timeout != 10*time.Second {
		t.Errorf("Timeout = %v, want 10s", opts.Timeout)
	}
}

// verifies SenderOptionsFromMap returns error when qos is out of range.
func TestSenderOptionsFromMap_InvalidQoS(t *testing.T) {
	m := map[string]any{"qos": 5}
	_, err := SenderOptionsFromMap(m)
	if err == nil {
		t.Error("expected error for out-of-range QoS")
	}
}

// verifies SenderOptionsFromMap returns error when qos has wrong type.
func TestSenderOptionsFromMap_QoSWrongType(t *testing.T) {
	m := map[string]any{"qos": "invalid"}
	_, err := SenderOptionsFromMap(m)
	if err == nil {
		t.Error("expected error for string QoS")
	}
}

// TestReceiverOptionsFromMap_Defaults validates that ReceiverOptionsFromMap
// returns a valid ReceiverOptions from an empty map.
func TestReceiverOptionsFromMap_Defaults(t *testing.T) {
	opts := ReceiverOptionsFromMap(nil)
	_ = opts
}

// TestReceiverOptionsFromMap_NonNilMap validates ReceiverOptionsFromMap
// with a populated options map (currently no receiver-specific options).
func TestReceiverOptionsFromMap_NonNilMap(t *testing.T) {
	opts := ReceiverOptionsFromMap(map[string]any{
		"some_unknown_key": "ignored",
	})
	_ = opts
}

// ═══════════════════════════════════════════════════════════════════════════
// BUG-10: keep_alive validation (0..65535)
//
// SessionOptionsFromMap must reject keep_alive values outside uint16 range
// and accept valid boundary values.
// ═══════════════════════════════════════════════════════════════════════════

// TestKeepAlive_ValidBoundary_Zero validates keep_alive=0 is accepted.
func TestKeepAlive_ValidBoundary_Zero(t *testing.T) {
	m := map[string]any{"keep_alive": 0}
	opts, err := SessionOptionsFromMap(m)
	if err != nil {
		t.Fatalf("keep_alive=0 should be valid, got error: %v", err)
	}
	if opts.KeepAlive != 0 {
		t.Errorf("KeepAlive = %d, want 0", opts.KeepAlive)
	}
}

// TestKeepAlive_ValidBoundary_Max validates keep_alive=65535 is accepted.
func TestKeepAlive_ValidBoundary_Max(t *testing.T) {
	m := map[string]any{"keep_alive": 65535}
	opts, err := SessionOptionsFromMap(m)
	if err != nil {
		t.Fatalf("keep_alive=65535 should be valid, got error: %v", err)
	}
	if opts.KeepAlive != 65535 {
		t.Errorf("KeepAlive = %d, want 65535", opts.KeepAlive)
	}
}

// TestKeepAlive_InvalidValues validates that out-of-range values are rejected.
func TestKeepAlive_InvalidValues(t *testing.T) {
	tests := []struct {
		name string
		val  int
	}{
		{"negative_minus_one", -1},
		{"negative_large", -100},
		{"too_large_70000", 70000},
		{"too_large_100000", 100000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{"keep_alive": tc.val}
			_, err := SessionOptionsFromMap(m)
			if err == nil {
				t.Errorf("expected error for keep_alive=%d, got nil", tc.val)
			}
		})
	}
}

// TestKeepAlive_NotProvided validates the default keep_alive (30).
func TestKeepAlive_NotProvided(t *testing.T) {
	m := map[string]any{"client_id": "test"}
	opts, err := SessionOptionsFromMap(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.KeepAlive != 30 {
		t.Errorf("KeepAlive = %d, want default 30", opts.KeepAlive)
	}
}

// TestKeepAlive_WrongType_String validates that a string value for keep_alive
// is rejected with a descriptive error.
func TestKeepAlive_WrongType_String(t *testing.T) {
	m := map[string]any{"keep_alive": "30"}
	_, err := SessionOptionsFromMap(m)
	if err == nil {
		t.Fatal("string keep_alive should return error")
	}
}

// TestKeepAlive_Float64 validates that a float64 value for keep_alive is
// accepted (JSON/YAML deserialize numbers as float64).
func TestKeepAlive_Float64(t *testing.T) {
	m := map[string]any{"keep_alive": 30.0}
	opts, err := SessionOptionsFromMap(m)
	if err != nil {
		t.Fatalf("float64 keep_alive should be accepted, got error: %v", err)
	}
	if opts.KeepAlive != 30 {
		t.Errorf("KeepAlive = %d, want 30", opts.KeepAlive)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// BUG-11: QoS type validation in SenderOptionsFromMap
//
// SenderOptionsFromMap must reject QoS values of the wrong type
// and out-of-range integer values.
// ═══════════════════════════════════════════════════════════════════════════

// TestQoS_ValidValues validates accepted QoS values 0, 1, 2.
func TestQoS_ValidValues(t *testing.T) {
	for _, qos := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("qos_%d", qos), func(t *testing.T) {
			m := map[string]any{"qos": qos}
			opts, err := SenderOptionsFromMap(m)
			if err != nil {
				t.Fatalf("QoS=%d should be valid, got error: %v", qos, err)
			}
			if opts.QoS != byte(qos) {
				t.Errorf("QoS = %d, want %d", opts.QoS, qos)
			}
		})
	}
}

// TestQoS_InvalidRange validates that out-of-range QoS int values are rejected.
func TestQoS_InvalidRange(t *testing.T) {
	tests := []struct {
		name string
		val  int
	}{
		{"negative", -1},
		{"three", 3},
		{"five", 5},
		{"hundred", 100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{"qos": tc.val}
			_, err := SenderOptionsFromMap(m)
			if err == nil {
				t.Errorf("expected error for QoS=%d, got nil", tc.val)
			}
		})
	}
}

// TestQoS_WrongType_String validates that QoS as string "1" returns error.
func TestQoS_WrongType_String(t *testing.T) {
	m := map[string]any{"qos": "1"}
	_, err := SenderOptionsFromMap(m)
	if err == nil {
		t.Fatal("expected error for string QoS, got nil")
	}
}

// TestQoS_Float64_ValidValue validates that QoS as float64 1.0 is accepted
// (JSON/YAML deserialize numbers as float64).
func TestQoS_Float64_ValidValue(t *testing.T) {
	m := map[string]any{"qos": 1.0}
	opts, err := SenderOptionsFromMap(m)
	if err != nil {
		t.Fatalf("float64 QoS=1.0 should be accepted, got error: %v", err)
	}
	if opts.QoS != 1 {
		t.Fatalf("expected QoS=1, got %d", opts.QoS)
	}
}

// TestQoS_WrongType_Bool validates that QoS as bool returns error.
func TestQoS_WrongType_Bool(t *testing.T) {
	m := map[string]any{"qos": true}
	_, err := SenderOptionsFromMap(m)
	if err == nil {
		t.Fatal("expected error for bool QoS, got nil")
	}
}

// TestQoS_NotProvided validates the default QoS (1).
func TestQoS_NotProvided(t *testing.T) {
	m := map[string]any{"default_topic": "test/topic"}
	opts, err := SenderOptionsFromMap(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.QoS != 1 {
		t.Errorf("QoS = %d, want default 1", opts.QoS)
	}
}

// TestQoS_Factory_NewSender_InvalidQoS validates that Factory.NewSender
// returns a proper error when QoS is invalid in the sender spec.
func TestQoS_Factory_NewSender_InvalidQoS(t *testing.T) {
	f := &Factory{}
	sess := validSession()

	tests := []struct {
		name string
		qos  any
	}{
		{"out_of_range", 5},
		{"string_type", "1"},
		{"negative", -1},
		{"float_out_of_range", 5.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.NewSender(context.Background(), ports.SenderSpec{
				ID:        "s-invalid-qos",
				SessionID: "test-session",
				Options:   map[string]any{"qos": tc.qos},
			}, sess)
			if err == nil {
				t.Errorf("expected error for QoS=%v (%T), got nil", tc.qos, tc.qos)
			}
		})
	}
}

// TestQoS_Factory_NewSender_ValidQoS validates that Factory.NewSender
// succeeds with valid QoS values.
func TestQoS_Factory_NewSender_ValidQoS(t *testing.T) {
	f := &Factory{}
	sess := validSession()

	for _, qos := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("qos_%d", qos), func(t *testing.T) {
			sender, err := f.NewSender(context.Background(), ports.SenderSpec{
				ID:        "s-valid-qos",
				SessionID: "test-session",
				Options:   map[string]any{"qos": qos},
			}, sess)
			if err != nil {
				t.Fatalf("QoS=%d should be valid, got error: %v", qos, err)
			}
			if sender == nil {
				t.Fatalf("sender should not be nil for QoS=%d", qos)
			}
		})
	}
}
