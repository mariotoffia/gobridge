package parser

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mariotoffia/gobridge/ports"
)

// Format specifies the configuration file format.
type Format string

const (
	FormatAuto Format = "auto"
	FormatYAML Format = "yaml"
	FormatJSON Format = "json"
)

// MaxConfigBytes is the maximum allowed configuration size (4 MiB).
// Inputs exceeding this limit are rejected to prevent excessive memory use.
const MaxConfigBytes = 4 << 20

// ParseFile loads and parses a configuration file using the supplied
// plugin registry. The format is detected from the file extension
// unless overridden by format. Supported extensions: .yaml, .yml
// (YAML), .json (JSON). registry MUST be non-nil; the composition
// root constructs a *ports.Registry and registers each adapter
// decoder explicitly via the adapter's exported Register function.
func ParseFile(path string, format Format, registry *ports.Registry) (*ports.BridgeConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if format == FormatAuto || format == "" {
		format = detectFormat(path)
	}

	return Parse(f, format, registry)
}

// Parse reads configuration from r using the specified format and the
// supplied plugin registry. Inputs larger than MaxConfigBytes are
// rejected. registry MUST be non-nil.
//
// The two-stage decode is:
//
//  1. yaml/json unmarshal into a stage-1 mirror struct that captures
//     each plugin attachment point's `options:` block as a raw
//     map[string]any;
//  2. for every attachment point, look up the registered decoder by
//     kind (Type/Transport, or inherited from parent for
//     SubscriptionDef/BindingDef), build a ports.RawConfig via
//     NewRawConfig, decode it through the registry, and stash both
//     the typed PluginConfig and the originating RawConfig on the
//     ports blueprint type via SetDecoded.
func Parse(r io.Reader, format Format, registry *ports.Registry) (*ports.BridgeConfig, error) {
	if registry == nil {
		return nil, fmt.Errorf("config: registry must not be nil")
	}

	lr := io.LimitReader(r, MaxConfigBytes+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("config: read: %w", err)
	}
	if len(data) > MaxConfigBytes {
		return nil, fmt.Errorf("config: input exceeds maximum size of %d bytes", MaxConfigBytes)
	}

	var s1 stage1Bridge
	switch format {
	case FormatJSON:
		if err := json.Unmarshal(data, &s1); err != nil {
			return nil, fmt.Errorf("config: json parse: %w", err)
		}
	case FormatYAML, FormatAuto, "":
		if err := yaml.Unmarshal(data, &s1); err != nil {
			return nil, fmt.Errorf("config: yaml parse: %w", err)
		}
	default:
		return nil, fmt.Errorf("config: unsupported format %q", format)
	}

	return s1.toBridgeConfig(registry)
}

func detectFormat(path string) Format {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".json":
		return FormatJSON
	case ".yaml", ".yml":
		return FormatYAML
	default:
		return FormatYAML
	}
}

// ─── stage-1 mirror struct ──────────────────────────────────────────
//
// stage1Bridge mirrors ports.BridgeConfig but keeps each plugin
// attachment point's `options:` block as a raw map[string]any so the
// stage-2 walker can route it through the plugin registry.

type stage1Bridge struct {
	Version     int                   `yaml:"version,omitempty" json:"version,omitempty"`
	Bridge      ports.BridgeSettings  `yaml:"bridge" json:"bridge"`
	ConfigWatch *ports.ConfigWatchDef `yaml:"config_watch,omitempty" json:"config_watch,omitempty"`
	Stores      stage1Stores          `yaml:"stores,omitempty" json:"stores,omitempty"`
	Sessions    []stage1Session       `yaml:"sessions,omitempty" json:"sessions,omitempty"`
	Receivers   []stage1Receiver      `yaml:"receivers,omitempty" json:"receivers,omitempty"`
	Senders     []stage1Sender        `yaml:"senders,omitempty" json:"senders,omitempty"`
	Bindings    []stage1Binding       `yaml:"bindings,omitempty" json:"bindings,omitempty"`
	Routes      []ports.RouteDef      `yaml:"routes,omitempty" json:"routes,omitempty"`
	HTTP        *ports.HTTPConfig     `yaml:"http,omitempty" json:"http,omitempty"`
}

type stage1Stores struct {
	Lease  *stage1Store `yaml:"lease,omitempty" json:"lease,omitempty"`
	Outbox *stage1Store `yaml:"outbox,omitempty" json:"outbox,omitempty"`
	DLQ    *stage1Store `yaml:"dlq,omitempty" json:"dlq,omitempty"`
}

type stage1Store struct {
	Type    string         `yaml:"type" json:"type"`
	Options map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
}

type stage1Session struct {
	ID          string         `yaml:"id" json:"id"`
	Transport   string         `yaml:"transport" json:"transport"`
	SessionMode string         `yaml:"session_mode,omitempty" json:"session_mode,omitempty"`
	Options     map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
}

type stage1Receiver struct {
	ID        string               `yaml:"id" json:"id"`
	Transport string               `yaml:"transport" json:"transport"`
	SessionID string               `yaml:"session_id,omitempty" json:"session_id,omitempty"`
	Topics    []stage1Subscription `yaml:"topics,omitempty" json:"topics,omitempty"`
	Options   map[string]any       `yaml:"options,omitempty" json:"options,omitempty"`
}

type stage1Subscription struct {
	Topic   string         `yaml:"topic" json:"topic"`
	QoS     int            `yaml:"qos,omitempty" json:"qos,omitempty"`
	Options map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
}

type stage1Sender struct {
	ID        string         `yaml:"id" json:"id"`
	Transport string         `yaml:"transport" json:"transport"`
	SessionID string         `yaml:"session_id,omitempty" json:"session_id,omitempty"`
	Options   map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
}

type stage1Binding struct {
	ID        string         `yaml:"id" json:"id"`
	SenderID  string         `yaml:"sender_id" json:"sender_id"`
	SessionID string         `yaml:"session_id,omitempty" json:"session_id,omitempty"`
	Address   string         `yaml:"address" json:"address"`
	Options   map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
}

// toBridgeConfig walks the stage-1 result and routes every plugin
// attachment point through registry, returning a fully populated
// *ports.BridgeConfig or the first parse error encountered.
func (s *stage1Bridge) toBridgeConfig(registry *ports.Registry) (*ports.BridgeConfig, error) {
	out := &ports.BridgeConfig{
		Version:     s.Version,
		Bridge:      s.Bridge,
		ConfigWatch: s.ConfigWatch,
		Routes:      s.Routes,
		HTTP:        s.HTTP,
	}

	if s.Stores.Lease != nil {
		sc, err := decodeStore(registry, "lease", s.Stores.Lease)
		if err != nil {
			return nil, err
		}
		out.Stores.Lease = sc
	}
	if s.Stores.Outbox != nil {
		sc, err := decodeStore(registry, "outbox", s.Stores.Outbox)
		if err != nil {
			return nil, err
		}
		out.Stores.Outbox = sc
	}
	if s.Stores.DLQ != nil {
		sc, err := decodeStore(registry, "dlq", s.Stores.DLQ)
		if err != nil {
			return nil, err
		}
		out.Stores.DLQ = sc
	}

	for _, s1 := range s.Sessions {
		sd, err := decodeSession(registry, s1)
		if err != nil {
			return nil, err
		}
		out.Sessions = append(out.Sessions, sd)
	}

	for _, r1 := range s.Receivers {
		rd, err := decodeReceiver(registry, r1)
		if err != nil {
			return nil, err
		}
		out.Receivers = append(out.Receivers, rd)
	}

	// senderKindByID lets us resolve the inherited kind for bindings.
	senderKindByID := make(map[string]string, len(s.Senders))
	for _, s1 := range s.Senders {
		sd, err := decodeSender(registry, s1)
		if err != nil {
			return nil, err
		}
		out.Senders = append(out.Senders, sd)
		senderKindByID[s1.ID] = s1.Transport
	}

	for _, b1 := range s.Bindings {
		bd, err := decodeBinding(registry, b1, senderKindByID)
		if err != nil {
			return nil, err
		}
		out.Bindings = append(out.Bindings, bd)
	}

	return out, nil
}

// decodePlugin is the common stage-2 routine. It builds a RawConfig
// from options, looks up the decoder by kind, and wraps any error
// with the user-facing attachment-point context. An empty options
// block still consults the registry: registered decoders see a
// zero-value RawConfig and can Validate accordingly.
func decodePlugin(registry *ports.Registry, attachment, id, kind string, options map[string]any) (ports.PluginConfig, ports.RawConfig, error) {
	if kind == "" {
		return nil, nil, fmt.Errorf("config: %s %q (kind %q): missing %s", attachment, id, kind, kindFieldName(attachment))
	}
	raw := NewRawConfig(options)
	cfg, err := registry.Decode(kind, raw)
	if err != nil {
		return nil, nil, fmt.Errorf("config: %s %q (kind %q): %w", attachment, id, kind, err)
	}
	return cfg, raw, nil
}

// kindFieldName returns the user-facing field name a stage-1 entry
// uses for its discriminator, so error messages say "missing type"
// for stores and "missing transport" everywhere else.
func kindFieldName(attachment string) string {
	if attachment == "store" {
		return "type"
	}
	return "transport"
}

func decodeStore(registry *ports.Registry, role string, s *stage1Store) (*ports.StoreConfig, error) {
	cfg, raw, err := decodePlugin(registry, "store", role, s.Type, s.Options)
	if err != nil {
		return nil, err
	}
	out := &ports.StoreConfig{Type: s.Type}
	out.SetDecoded(cfg, raw)
	return out, nil
}

func decodeSession(registry *ports.Registry, s stage1Session) (ports.SessionDef, error) {
	cfg, raw, err := decodePlugin(registry, "session", s.ID, s.Transport, s.Options)
	if err != nil {
		return ports.SessionDef{}, err
	}
	out := ports.SessionDef{
		ID:          s.ID,
		Transport:   s.Transport,
		SessionMode: s.SessionMode,
	}
	out.SetDecoded(cfg, raw)
	return out, nil
}

func decodeReceiver(registry *ports.Registry, r stage1Receiver) (ports.ReceiverDef, error) {
	cfg, raw, err := decodePlugin(registry, "receiver", r.ID, r.Transport, r.Options)
	if err != nil {
		return ports.ReceiverDef{}, err
	}
	out := ports.ReceiverDef{
		ID:        r.ID,
		Transport: r.Transport,
		SessionID: r.SessionID,
	}
	out.SetDecoded(cfg, raw)

	// SubscriptionDef inherits its kind from the parent receiver's
	// transport; subscriptions have no own discriminator.
	if len(r.Topics) > 0 {
		out.Topics = make([]ports.SubscriptionDef, 0, len(r.Topics))
		for i, t := range r.Topics {
			subID := fmt.Sprintf("%s/topics[%d]", r.ID, i)
			scfg, sraw, err := decodePlugin(registry, "subscription", subID, r.Transport, t.Options)
			if err != nil {
				return ports.ReceiverDef{}, err
			}
			sub := ports.SubscriptionDef{Topic: t.Topic, QoS: t.QoS}
			sub.SetDecoded(scfg, sraw)
			out.Topics = append(out.Topics, sub)
		}
	}
	return out, nil
}

func decodeSender(registry *ports.Registry, s stage1Sender) (ports.SenderDef, error) {
	cfg, raw, err := decodePlugin(registry, "sender", s.ID, s.Transport, s.Options)
	if err != nil {
		return ports.SenderDef{}, err
	}
	out := ports.SenderDef{
		ID:        s.ID,
		Transport: s.Transport,
		SessionID: s.SessionID,
	}
	out.SetDecoded(cfg, raw)
	return out, nil
}

func decodeBinding(registry *ports.Registry, b stage1Binding, senderKindByID map[string]string) (ports.BindingDef, error) {
	kind, ok := senderKindByID[b.SenderID]
	if !ok {
		return ports.BindingDef{}, fmt.Errorf("config: binding %q: sender %q not found", b.ID, b.SenderID)
	}
	cfg, raw, err := decodePlugin(registry, "binding", b.ID, kind, b.Options)
	if err != nil {
		return ports.BindingDef{}, err
	}
	out := ports.BindingDef{
		ID:        b.ID,
		SenderID:  b.SenderID,
		SessionID: b.SessionID,
		Address:   b.Address,
	}
	out.SetDecoded(cfg, raw)
	return out, nil
}
