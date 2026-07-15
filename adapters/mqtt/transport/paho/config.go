package paho

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// SessionOptions holds MQTT connection and session configuration.
// These values are typically extracted from ports.SessionSpec.Options.
type SessionOptions struct {
	BrokerURLs []string `mapstructure:"broker_urls" yaml:"broker_urls" json:"broker_urls"`
	// BrokerURL is the single-broker convenience form of broker_urls and the
	// dominant documented key (configuration-reference.md, scenarios 01-17).
	// normalizeBrokerURLs folds it into BrokerURLs when the list form is absent
	// and clears it, so the typed decode path honors the documented
	// single-broker config and re-serializes on the canonical broker_urls key.
	BrokerURL string `mapstructure:"broker_url" yaml:"broker_url,omitempty" json:"broker_url,omitempty"`
	ClientID  string `mapstructure:"client_id" yaml:"client_id" json:"client_id"`
	// ClientIDSuffix, when set, appends a per-replica-unique token to
	// ClientID at build time so N replicas sharing ONE config file (a
	// Kubernetes Deployment or ECS service with replicas>1 and a single
	// ConfigMap) get DISTINCT client_ids — which $share scale-out REQUIRES —
	// without per-pod config templating (M-3). Supported tokens:
	//   - "hostname": append "-<os.Hostname()>" (the pod name under K8s, the
	//     task id under ECS). STABLE across restarts of the same replica, so
	//     it is the preferred choice and a resumed Persistent session keeps
	//     its identity.
	//   - "nonce": append "-<random>", a fresh value each process start. Use
	//     for Ephemeral $share workers where per-restart identity churn is
	//     acceptable (a Persistent/Exclusive session would orphan its prior
	//     broker session on every restart).
	// It is REJECTED (build fails) for session_mode: exclusive, whose
	// identity contract requires a STABLE, SHARED client_id across instances:
	// the lease serialises connections and the standby resumes the broker
	// session on takeover, so a unique-per-instance id would strand queued
	// QoS messages (see acl_session.go and scenario 08 / H-1). Empty means
	// ClientID is used verbatim (the default).
	ClientIDSuffix string `mapstructure:"client_id_suffix" yaml:"client_id_suffix,omitempty" json:"client_id_suffix,omitempty"`
	// KeepAlive is the MQTT keep-alive interval in seconds. The registry
	// decode path pre-fills the documented default (30) before decoding,
	// so an omitted key gets 30 while an EXPLICIT `keep_alive: 0`
	// (disable the pinger) is honoured. Hand-built SessionOptions carry
	// the caller's literal value unchanged.
	KeepAlive uint16 `mapstructure:"keep_alive" yaml:"keep_alive" json:"keep_alive"`
	// ConnectTimeout bounds the INITIAL Start connection await. Zero
	// falls back to 30s at Start time.
	ConnectTimeout time.Duration `mapstructure:"connect_timeout" yaml:"connect_timeout" json:"connect_timeout"`
	// ReconnectTimeout bounds each individual (re)connect attempt made
	// by the autopaho reconnection loop (TCP dial + TLS handshake + MQTT
	// CONNECT/CONNACK). Zero means the autopaho default (10s). It maps
	// to autopaho.ClientConfig.ConnectTimeout.
	ReconnectTimeout time.Duration `mapstructure:"reconnect_timeout" yaml:"reconnect_timeout" json:"reconnect_timeout"`
	// ReconcileTimeout bounds EACH broker SUBSCRIBE / UNSUBSCRIBE the
	// session manager issues while reconciling the SessionPlan. The
	// reconcile loop runs on the runtime's context, which may carry no
	// deadline, so without an adapter-owned timeout a wedged broker (a
	// SUBSCRIBE whose SUBACK never arrives on a half-open connection) hangs
	// the reconcile — and any startup/hot-reload step awaiting it —
	// indefinitely (HIGH-2). Each broker op is wrapped in
	// context.WithTimeout(ctx, reconcile_timeout) so it fails fast and the
	// caller can retry on the next reconcile. A non-positive value is
	// coerced to DefaultReconcileTimeout (30s): unlike the tuning knobs
	// that honour an explicit 0, this is a liveness safety bound that must
	// not be disabled (a disabled reconcile timeout re-introduces the hang).
	ReconcileTimeout time.Duration `mapstructure:"reconcile_timeout" yaml:"reconcile_timeout" json:"reconcile_timeout"`
	// CleanStart is the MQTT 5 clean-start flag consulted for Persistent
	// and Exclusive sessions (Ephemeral sessions always connect with
	// CleanStart=true regardless). The default is FALSE: the modes that
	// consult the flag exist to RESUME broker session state, so a
	// clean-start default would silently discard it.
	CleanStart bool `mapstructure:"clean_start" yaml:"clean_start" json:"clean_start"`
	// SessionExpiryInterval is the MQTT 5 session expiry in seconds.
	// For Persistent/Exclusive sessions a zero value gives ZERO offline
	// retention (the broker drops the session state the moment the
	// connection closes), contradicting the mode's purpose — NewSession
	// therefore defaults zero to DefaultPersistentSessionExpiry for
	// those modes (with a warning). Ephemeral sessions always use 0.
	SessionExpiryInterval uint32        `mapstructure:"session_expiry_interval" yaml:"session_expiry_interval" json:"session_expiry_interval"`
	Username              string        `mapstructure:"username" yaml:"username" json:"username"`
	Password              shared.Secret `mapstructure:"password" yaml:"password" json:"password"`
	TLS                   *TLSConfig    `mapstructure:"tls" yaml:"tls" json:"tls"`
	// AllowPlaintextCredentials opts IN to sending the MQTT CONNECT
	// username/password over a NON-TLS broker URL (tcp://, mqtt://, ws://,
	// or a schemeless URL), where autopaho dials in cleartext and the
	// credentials travel on the wire in the clear (HIGH-4). It defaults
	// FALSE and the adapter FAILS CLOSED when credentials are configured
	// without every broker_urls entry using a TLS scheme (ssl://, mqtts://,
	// tls://, mqtt+ssl://, tcps://, wss://). Note that tls.enable does NOT
	// change this: autopaho selects TLS purely from the URL scheme, so a
	// tcp:// URL is cleartext regardless of tls settings. Set true only for
	// trusted transports (a private mesh, mTLS-terminating sidecar, or a
	// localhost test broker) where the plaintext exposure is acceptable.
	AllowPlaintextCredentials bool `mapstructure:"allow_plaintext_credentials" yaml:"allow_plaintext_credentials" json:"allow_plaintext_credentials"`
	// Will configures an MQTT Last Will and Testament published by the
	// broker when this session's connection terminates ungracefully,
	// letting peers detect ungraceful death. Optional; nil means no will.
	Will *WillOptions `mapstructure:"will" yaml:"will,omitempty" json:"will,omitempty"`
	// ReceiveMaximum sets the MQTT v5 Receive Maximum property in the
	// CONNECT packet. This limits the number of QoS 1/2 messages the
	// broker may have in flight (sent but not yet PUBACK/PUBCOMP-settled)
	// at once. Zero means "unset" and is coerced by NewSession to
	// DefaultReceiveMaximum (192) — 0 is not a legal MQTT v5 Receive Maximum.
	//
	// Receive Maximum also sizes the serialized dispatch queue and startup
	// pending-entry cap. The full byte equation, including route concurrency,
	// is enforced by ValidateIngressMemory.
	ReceiveMaximum uint16 `mapstructure:"receive_maximum" yaml:"receive_maximum" json:"receive_maximum"`
	// MaxPayloadBytes is the maximum application PAYLOAD size (message body, in
	// bytes) this session will admit from the broker. Zero selects
	// DefaultMaxPayloadBytes (256 KiB). The adapter
	// advertises an MQTT v5 Maximum Packet Size in the CONNECT derived from this
	// value plus the bounded worst-case protocol metadata allowance. A local
	// callback guard rejects a non-compliant broker's oversize body before the
	// adapter copies or enqueues it. This is an inbound limit only.
	MaxPayloadBytes uint32 `mapstructure:"max_payload_bytes" yaml:"max_payload_bytes" json:"max_payload_bytes"`
	// IngressMemoryBudgetBytes is the maximum conservative byte bound for this
	// session's inbound MQTT packet window. Zero selects
	// DefaultIngressMemoryBudgetBytes. The bridge validates the effective route
	// concurrency against this budget before opening stores or transports.
	IngressMemoryBudgetBytes uint64 `mapstructure:"ingress_memory_budget_bytes" yaml:"ingress_memory_budget_bytes" json:"ingress_memory_budget_bytes"`
	// ingressDefaultsApplied distinguishes decoder-prefilled defaults from
	// non-zero values supplied by a programmatic caller. The AWS deployment
	// profile uses the explicit markers to reject an unsafe operator value
	// instead of silently reducing it.
	ingressDefaultsApplied      bool `mapstructure:"-" yaml:"-" json:"-"`
	receiveMaximumExplicit      bool `mapstructure:"-" yaml:"-" json:"-"`
	ingressMemoryBudgetExplicit bool `mapstructure:"-" yaml:"-" json:"-"`
	// ReconnectDelay is the BASE delay for the jittered exponential
	// reconnect backoff (M-4): the delay before the first reconnect
	// attempt after a failure, grown by reconnectBackoffFactor per
	// subsequent attempt up to ReconnectMaxDelay, then equal-jittered to
	// spread a reconnecting fleet (anti thundering-herd). Zero uses
	// DefaultReconnectDelay (10s). Shorter values speed up reconnection in
	// test environments but increase load on the broker in production.
	ReconnectDelay time.Duration `mapstructure:"reconnect_delay" yaml:"reconnect_delay" json:"reconnect_delay"`
	// ReconnectMaxDelay caps the jittered-exponential reconnect backoff
	// envelope (M-4). The per-attempt base delay grows from ReconnectDelay
	// up to this ceiling; equal-jitter is then applied within the envelope.
	// Zero uses DefaultReconnectMaxDelay (2m). Must be >= ReconnectDelay;
	// a smaller value is clamped up to the base at Start.
	ReconnectMaxDelay time.Duration `mapstructure:"reconnect_max_delay" yaml:"reconnect_max_delay" json:"reconnect_max_delay"`
	// UnmatchedGrace bounds the startup window during which an incoming
	// publish that matches NO registered receiver filter is BUFFERED
	// (un-acked) rather than acked-and-dropped. It exists because on a
	// resumed clean_start=false session the broker starts delivering the
	// queued backlog on CONNACK before the route runners have registered
	// their topic filters (the runtime session manager Starts, then
	// Reconciles, then the receivers register — see runtime/session).
	// After the grace window a still-unmatched publish is treated as an
	// orphan broker subscription: acked, dropped, and its exact topic
	// unsubscribed (see acl_router.go / doc.go). The window RESTARTS on
	// every (re)connect. Zero falls back to DefaultUnmatchedGrace (30s).
	UnmatchedGrace time.Duration `mapstructure:"unmatched_grace" yaml:"unmatched_grace" json:"unmatched_grace"`
	// NoLocal opts IN to MQTT 5 No-Local delivery (§3.8.3.1) on every
	// ordinary subscription this session issues: the broker is asked NOT to
	// deliver a message back to the connection that published it.
	//
	// It exists for the same-broker MQTT->MQTT bridge topology where ONE
	// session both subscribes and publishes on OVERLAPPING filters — e.g. a
	// route that consumes `sensors/#` and republishes under `sensors/archive/…`
	// on the SAME session. Without No-Local the broker echoes each republish
	// back to the session's own `sensors/#` subscription, which re-forwards it,
	// an unbounded self-amplification loop at line rate (Scenario 01). Turning
	// this on breaks that loop at the broker.
	//
	// It defaults FALSE to preserve the least-surprising MQTT delivery
	// contract: a session receiving its OWN publishes is legitimate (request/
	// reply on one client, a loopback verifier), and only the overlap-plus-
	// republish topology loops. Enable it only on a session whose subscribe
	// filters overlap its own publish topics. Cross-bridge delivery is never
	// affected — No-Local is per-connection and distinct bridges use distinct
	// client_ids. A shared subscription ($share) ALWAYS ignores this flag:
	// No-Local on $share is an MQTT 5 Protocol Error the broker rejects with a
	// DISCONNECT, so reconcile forces it off there regardless.
	NoLocal bool `mapstructure:"no_local" yaml:"no_local" json:"no_local"`
	// Clock is an internal dependency injected by the factory/tests and
	// must never be populated from YAML; the dash tag excludes it from
	// the strict options decoder (which would otherwise reject it).
	Clock clock.Clock `mapstructure:"-" yaml:"-" json:"-"`
}

// schemeUsesTLS reports whether an MQTT broker URL's scheme selects a TLS
// transport. autopaho (paho.golang autopaho/net.go) dials TLS ONLY for these
// schemes; every other scheme (tcp, mqtt, ws, or schemeless) dials in
// CLEARTEXT — and the CONNECT packet's username/password travel in the clear.
// Note: cfg.TlsCfg is applied by autopaho ONLY on a TLS scheme, so tls.enable
// on a tcp:// URL is a silent no-op (HIGH-4).
func schemeUsesTLS(brokerURL string) bool {
	u, err := url.Parse(brokerURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "ssl", "tls", "mqtts", "mqtt+ssl", "tcps", "wss":
		return true
	default:
		return false
	}
}

// allBrokerURLsUseTLS reports whether EVERY configured broker URL uses a TLS
// scheme. A single plaintext URL in the list is a cleartext-credential vector
// (the reconnect loop may dial any of them), so the security gate treats the
// weakest link as the effective transport security. An empty list is not TLS.
func allBrokerURLsUseTLS(urls []string) bool {
	if len(urls) == 0 {
		return false
	}
	for _, u := range urls {
		if !schemeUsesTLS(u) {
			return false
		}
	}
	return true
}

// hasCredentials reports whether the session carries MQTT CONNECT
// authentication material (username or password). MQTT sends these in the
// CONNECT packet in cleartext, so they are the credentials the HIGH-4 gate
// protects. Unlike AMQP, autopaho does NOT source credentials from broker-URL
// userinfo for the CONNECT — only these fields — so the gate checks them alone.
func (o *SessionOptions) hasCredentials() bool {
	return o.Username != "" || !o.Password.IsZero()
}

// plaintextCredentialViolation is the single source of truth for the HIGH-4
// cleartext-credential gate: it reports whether sending the given credential
// state over the given broker URLs would put username/password on the wire in
// the clear without the explicit allow_plaintext_credentials opt-in. Both the
// static-config validator (validatePlaintextCredentials) and the RUNTIME
// credential-rotation gate (Session.ApplyCredentials) AND the dial-time
// defense-in-depth guard route through this predicate so all three decide
// identically. An empty broker-URL list is "transport unknown" and never
// violates here — the factory re-validates once broker_urls are populated.
func plaintextCredentialViolation(hasCreds, allowPlaintext bool, brokerURLs []string) bool {
	if len(brokerURLs) == 0 {
		return false
	}
	return hasCreds && !allowPlaintext && !allBrokerURLsUseTLS(brokerURLs)
}

// errPlaintextCredentials is the fail-closed error every HIGH-4 gate returns so
// the message (and its BridgeError code) is identical whether the violation is
// caught at static config validation, at runtime credential rotation, or at the
// dial-time defense-in-depth guard.
func errPlaintextCredentials() error {
	return shared.ErrInvalidPayload.WithMessage(
		"mqtt: username/password are sent in the MQTT CONNECT packet in cleartext but not all broker_urls " +
			"use a TLS scheme; use ssl:// (or mqtts://, tls://, mqtt+ssl://, tcps://, wss://), or set " +
			"allow_plaintext_credentials=true to send credentials in cleartext anyway")
}

// validatePlaintextCredentials fails closed when username/password credentials
// are configured but NOT every broker URL uses a TLS scheme (HIGH-4). The MQTT
// CONNECT packet carries username/password in cleartext, so on a tcp:// (or
// other non-TLS) broker they travel on the wire in the clear. It is the
// explicit-opt-in gate: allow_plaintext_credentials=true is the escape hatch
// for trusted transports. The check is skipped when no broker URLs are known
// yet (hand-built options validated before the factory populates them, or a
// receiver/sender spec that inherits its connection identity from a session);
// the factory re-runs it once broker_urls are populated.
func (o *SessionOptions) validatePlaintextCredentials() error {
	if plaintextCredentialViolation(o.hasCredentials(), o.AllowPlaintextCredentials, o.BrokerURLs) {
		return errPlaintextCredentials()
	}
	return nil
}

// normalizeBrokerURLs folds the single-broker BrokerURL alias into the
// canonical BrokerURLs list when the list form is empty, then clears the alias
// so a re-serialized config carries only broker_urls. The registry decoder
// calls this after Decode and before Validate.
func (o *SessionOptions) normalizeBrokerURLs() {
	if o.BrokerURL != "" && len(o.BrokerURLs) == 0 {
		o.BrokerURLs = []string{o.BrokerURL}
	}
	o.BrokerURL = ""
}

// ReceiverOptions holds MQTT receiver-specific configuration.
type ReceiverOptions struct {
	// No additional fields; subscriptions come from ReceiverSpec.Subscriptions.
}

// WillOptions configures the MQTT Last Will and Testament (LWT) for a
// session. The broker publishes the will message on the configured
// topic when the client's connection terminates ungracefully (network
// partition, crash, keep-alive timeout) — peers subscribe to the will
// topic to detect ungraceful death. A graceful Close (normal
// DISCONNECT) does not trigger the will.
type WillOptions struct {
	Topic string `mapstructure:"topic" yaml:"topic" json:"topic"`
	// Payload is the will message body. It is a string, so only UTF-8 /
	// text wills are expressible through config today.
	//
	// ponytail: binary will payloads are not supported via config. A
	// base64-decoded `payload_base64` key (decoded into a []byte will body)
	// is the deferred option; it was left out here to avoid widening the
	// config schema and the decode/validation/precedence surface for a
	// rarely-used path. Callers needing a binary will can construct
	// WillOptions programmatically once the field is widened.
	Payload string `mapstructure:"payload" yaml:"payload" json:"payload"`
	QoS     byte   `mapstructure:"qos" yaml:"qos" json:"qos"`
	Retain  bool   `mapstructure:"retain" yaml:"retain" json:"retain"`
}

// Validate checks the will configuration for protocol violations.
func (w *WillOptions) Validate() error {
	if w == nil {
		return nil
	}
	if w.Topic == "" {
		return fmt.Errorf("mqtt: will.topic is required when will is configured")
	}
	if strings.ContainsAny(w.Topic, "+#") {
		return fmt.Errorf("mqtt: will.topic %q must not contain wildcards", w.Topic)
	}
	if w.QoS > 2 {
		return fmt.Errorf("mqtt: will.qos must be 0, 1, or 2, got %d", w.QoS)
	}
	return nil
}

// SenderOptions holds MQTT sender-specific configuration.
type SenderOptions struct {
	DefaultTopic       string        `mapstructure:"default_topic" yaml:"default_topic" json:"default_topic"`
	QoS                byte          `mapstructure:"qos" yaml:"qos" json:"qos"`
	Retain             bool          `mapstructure:"retain" yaml:"retain" json:"retain"`
	Timeout            time.Duration `mapstructure:"timeout" yaml:"timeout" json:"timeout"`
	ThrottleRetryAfter time.Duration `mapstructure:"throttle_retry_after" yaml:"throttle_retry_after" json:"throttle_retry_after"`
}

// TLSConfig holds TLS settings for the MQTT connection.
//
// Two parallel sources of material are supported: file paths (loaded
// from disk at connect time) and PEM byte strings (used in-memory).
// PEM strings take precedence when both are set on the same field,
// because the push-credentials path delivers PEM material and a
// non-empty PEM should never be silently ignored in favour of a
// stale file on disk.
type TLSConfig struct {
	Enable     bool   `mapstructure:"enable" yaml:"enable" json:"enable"`
	CACertFile string `mapstructure:"ca_cert_file" yaml:"ca_cert_file" json:"ca_cert_file"`
	CertFile   string `mapstructure:"cert_file" yaml:"cert_file" json:"cert_file"`
	KeyFile    string `mapstructure:"key_file" yaml:"key_file" json:"key_file"`

	// CACertPEM, CertPEM, KeyPEM carry in-memory PEM material, typically
	// populated by credential rotation. When any of these is non-empty
	// it takes precedence over the corresponding *File field. They are
	// shared.Secret so the (sensitive) private-key material — and, for
	// uniformity, the cert/CA bundles — redact on JSON/YAML/log marshal;
	// the config-save path reveals explicitly (see shared.RevealSecrets).
	CACertPEM shared.Secret `mapstructure:"ca_cert_pem" yaml:"ca_cert_pem" json:"ca_cert_pem"`
	CertPEM   shared.Secret `mapstructure:"cert_pem" yaml:"cert_pem" json:"cert_pem"`
	KeyPEM    shared.Secret `mapstructure:"key_pem" yaml:"key_pem" json:"key_pem"`

	InsecureSkipVerify bool `mapstructure:"insecure_skip_verify" yaml:"insecure_skip_verify" json:"insecure_skip_verify"`
}

// DefaultPersistentSessionExpiry is the session_expiry_interval (in
// seconds) applied by NewSession when a Persistent or Exclusive session
// is configured with the zero value. 0 would give ZERO offline
// retention — the broker drops subscriptions and queued QoS 1/2
// messages the instant the connection closes — contradicting the
// documented purpose of those modes. 24 hours survives typical
// restarts and outages without leaking broker session state forever
// (0xFFFFFFFF "never expire" was deliberately rejected).
const DefaultPersistentSessionExpiry uint32 = 86400

// DefaultConnectTimeout bounds the initial connection await when
// connect_timeout is unset or zero.
const DefaultConnectTimeout = 30 * time.Second

// DefaultReconnectAttemptTimeout is autopaho's effective per-attempt timeout
// when reconnect_timeout is explicitly zero (an omitted decoded config keeps the
// documented 30s value from DefaultSessionOptions).
const DefaultReconnectAttemptTimeout = 10 * time.Second

// DefaultUnmatchedGrace is the UnmatchedGrace applied by NewSession /
// the router when the session does not configure unmatched_grace. It is
// sized to comfortably cover the legitimate startup window on a resumed
// (clean_start=false) session: the broker starts pushing the queued
// backlog on CONNACK, while the runtime session manager is still walking
// Start → Reconcile → receiver registration, so briefly some publishes
// match no handler yet. 30s absorbs that reconciliation without letting
// a genuine orphan subscription pin the broker's in-flight window for
// long.
const DefaultUnmatchedGrace = 30 * time.Second

const (
	// DefaultMaxPayloadBytes is the effective inbound application-payload
	// ceiling when max_payload_bytes is zero.
	DefaultMaxPayloadBytes uint32 = 256 << 10
	// DefaultReceiveMaximum is the effective MQTT v5 Receive Maximum when
	// receive_maximum is zero.
	DefaultReceiveMaximum uint16 = 192
	// DefaultIngressMemoryBudgetBytes is the per-session conservative ingress
	// memory budget when ingress_memory_budget_bytes is zero.
	DefaultIngressMemoryBudgetBytes uint64 = 256 << 20
)

// mqttPacketOverheadAllowance is the maximum metadata allowance admitted in
// addition to a full max_payload_bytes body. The 128 KiB allowance includes the
// MQTT v5 PUBLISH fixed-header byte, the worst-case four-byte Remaining Length
// encoding, the two-byte topic-length prefix plus a 65,535-byte topic, a
// two-byte QoS packet identifier, the worst-case four-byte properties-length
// encoding, and 65,524 bytes of properties. A packet with a smaller body may
// use more metadata, but Maximum Packet Size still caps the whole admitted
// packet at max_payload_bytes + this allowance, so the byte model never
// undercounts an admitted packet.
const mqttPacketOverheadAllowance uint64 = 128 << 10

// mqttMaxPacketSize is the MQTT v5 Maximum Packet Size ceiling: 256 MiB - 1.
const mqttMaxPacketSize uint64 = 268_435_455

const (
	// maxIngressUserProperties bounds per-property Go struct amplification for
	// packets retained beyond the Paho callback.
	maxIngressUserProperties = 128
	// maxIngressMetadataBytes bounds encoded topic/properties metadata retained
	// by an accepted packet.
	maxIngressMetadataBytes uint64 = mqttPacketOverheadAllowance
	// retainedUserPropertyBytes covers the Paho wire-packet and callback
	// UserProperty structs retained simultaneously for one formula slot.
	retainedUserPropertyBytes uint64 = 64
	// retainedPacketFixedBytes covers Publish/Properties structs, accepted
	// Envelope header-map buckets, outbox/queue item state, and allocator
	// page/size-class rounding observed by the finite-cgroup proof.
	retainedPacketFixedBytes uint64 = 32 << 10
)

// wirePacketSizeFor returns the MQTT v5 Maximum Packet Size advertised to the
// broker.
func wirePacketSizeFor(maxPayloadBytes uint32) (uint32, error) {
	if uint64(maxPayloadBytes) > mqttMaxPacketSize-mqttPacketOverheadAllowance {
		return 0, shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"mqtt: max_payload_bytes %d exceeds the largest value %d that fits the MQTT v5 packet ceiling with metadata overhead",
			maxPayloadBytes, mqttMaxPacketSize-mqttPacketOverheadAllowance,
		))
	}
	return uint32(uint64(maxPayloadBytes) + mqttPacketOverheadAllowance), nil
}

// maxPacketSizeFor returns a conservative retained-heap base for one accepted
// packet representation. The wire limit is augmented by the accepted
// UserProperty structural cap and fixed Go/queue representation allowance.
func maxPacketSizeFor(maxPayloadBytes uint32) (uint32, error) {
	wire, err := wirePacketSizeFor(maxPayloadBytes)
	if err != nil {
		return 0, err
	}
	retained := uint64(wire) +
		maxIngressUserProperties*retainedUserPropertyBytes +
		retainedPacketFixedBytes
	if retained > math.MaxUint32 {
		return 0, shared.ErrInvalidConfig.WithMessage("mqtt: retained packet memory exceeds uint32")
	}
	return uint32(retained), nil
}

// ingressMemoryPacketBytes returns
// ceil(maxPacketSize(maxPayloadBytes) * 1.25). The 25 percent accounts for Go
// object, slice, queue-node, and SDK bookkeeping around the complete admitted
// MQTT packet.
func ingressMemoryPacketBytes(maxPayloadBytes uint32) (uint64, error) {
	if maxPayloadBytes == 0 {
		maxPayloadBytes = DefaultMaxPayloadBytes
	}
	packetSize, err := maxPacketSizeFor(maxPayloadBytes)
	if err != nil {
		return 0, err
	}
	packet := uint64(packetSize)
	quarter := packet / 4
	if packet%4 != 0 {
		var addErr error
		quarter, addErr = checkedAddUint64(quarter, 1)
		if addErr != nil {
			return 0, addErr
		}
	}
	return checkedAddUint64(packet, quarter)
}

// IngressMemoryBound calculates the conservative per-session byte bound:
//
//	packet = ceil(maxPacketSize(maxPayloadBytes) * 1.25)
//	window = receiveMaximum + dispatchCapacity + routeMaxInFlight + 1
//	bound  = packet * window
//
// Dispatch capacity equals the effective Receive Maximum. Zero payload and
// receive values select the adapter defaults.
func IngressMemoryBound(
	maxPayloadBytes uint32,
	receiveMaximum uint16,
	routeMaxInFlight uint64,
) (uint64, error) {
	if receiveMaximum == 0 {
		receiveMaximum = DefaultReceiveMaximum
	}
	packet, err := ingressMemoryPacketBytes(maxPayloadBytes)
	if err != nil {
		return 0, err
	}
	window, err := checkedAddUint64(uint64(receiveMaximum), uint64(receiveMaximum))
	if err != nil {
		return 0, err
	}
	window, err = checkedAddUint64(window, routeMaxInFlight)
	if err != nil {
		return 0, err
	}
	window, err = checkedAddUint64(window, 1)
	if err != nil {
		return 0, err
	}
	return checkedMulUint64(packet, window)
}

// LargestSafeReceiveMaximum derives the largest legal MQTT v5 Receive Maximum
// whose ingress-memory bound fits budgetBytes for the supplied payload and route
// concurrency. It rejects a budget that cannot fit one receive slot, one
// dispatch slot, the route window, and the current packet.
func LargestSafeReceiveMaximum(
	maxPayloadBytes uint32,
	budgetBytes uint64,
	routeMaxInFlight uint64,
) (uint16, error) {
	packet, err := ingressMemoryPacketBytes(maxPayloadBytes)
	if err != nil {
		return 0, err
	}
	if budgetBytes == 0 {
		return 0, shared.ErrInvalidConfig.WithMessage("mqtt: ingress memory budget must be greater than zero")
	}
	maxWindow := budgetBytes / packet
	baseWindow, err := checkedAddUint64(routeMaxInFlight, 1)
	if err != nil {
		return 0, err
	}
	minWindow, err := checkedAddUint64(baseWindow, 2)
	if err != nil {
		return 0, err
	}
	if maxWindow < minWindow {
		minimumBytes, mulErr := checkedMulUint64(packet, minWindow)
		if mulErr != nil {
			return 0, mulErr
		}
		return 0, shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"mqtt: ingress memory budget %d bytes is too small for one receive packet (requires at least %d bytes)",
			budgetBytes, minimumBytes,
		))
	}
	receive := (maxWindow - baseWindow) / 2
	if receive > math.MaxUint16 {
		receive = math.MaxUint16
	}
	if receive == 0 {
		return 0, shared.ErrInvalidConfig.WithMessage("mqtt: ingress memory budget cannot fit a legal Receive Maximum")
	}
	bound, err := IngressMemoryBound(maxPayloadBytes, uint16(receive), routeMaxInFlight)
	if err != nil {
		return 0, err
	}
	if bound > budgetBytes {
		return 0, shared.ErrInvalidConfig.WithMessage("mqtt: derived Receive Maximum exceeds ingress memory budget")
	}
	return uint16(receive), nil
}

func checkedAddUint64(a, b uint64) (uint64, error) {
	if b > math.MaxUint64-a {
		return 0, shared.ErrInvalidConfig.WithMessage("mqtt: ingress memory calculation overflows integer addition")
	}
	return a + b, nil
}

func checkedMulUint64(a, b uint64) (uint64, error) {
	if a != 0 && b > math.MaxUint64/a {
		return 0, shared.ErrInvalidConfig.WithMessage("mqtt: ingress memory calculation overflows integer multiplication")
	}
	return a * b, nil
}

func (o SessionOptions) normalizedIngressMemory() SessionOptions {
	out := o
	if !out.ingressDefaultsApplied {
		out.receiveMaximumExplicit = out.ReceiveMaximum != 0
		out.ingressMemoryBudgetExplicit = out.IngressMemoryBudgetBytes != 0
	} else {
		if out.ReceiveMaximum != 0 && out.ReceiveMaximum != DefaultReceiveMaximum {
			out.receiveMaximumExplicit = true
		}
		if out.IngressMemoryBudgetBytes != 0 &&
			out.IngressMemoryBudgetBytes != DefaultIngressMemoryBudgetBytes {
			out.ingressMemoryBudgetExplicit = true
		}
	}
	if out.MaxPayloadBytes == 0 {
		out.MaxPayloadBytes = DefaultMaxPayloadBytes
	}
	if out.ReceiveMaximum == 0 {
		out.ReceiveMaximum = DefaultReceiveMaximum
	}
	if out.IngressMemoryBudgetBytes == 0 {
		out.IngressMemoryBudgetBytes = DefaultIngressMemoryBudgetBytes
	}
	out.ingressDefaultsApplied = true
	return out
}

// Reconnect-backoff defaults (M-4). autopaho's reconnect loop calls
// ReconnectBackoff(attempt) before each (re)connect; attempt 0 (the first
// try) is always 0 delay. From attempt 1 the base delay grows
// exponentially by reconnectBackoffFactor up to DefaultReconnectMaxDelay,
// then equal-jitter spreads a reconnecting fleet so instances that all
// lost a shared broker do not retry in lockstep (thundering herd).
const (
	// DefaultReconnectDelay is the base (attempt-1) reconnect delay when
	// reconnect_delay is unset. Matches the historical autopaho default.
	DefaultReconnectDelay = 10 * time.Second
	// DefaultReconnectMaxDelay caps the reconnect-backoff envelope when
	// reconnect_max_delay is unset.
	DefaultReconnectMaxDelay = 2 * time.Minute
	// reconnectBackoffFactor is the per-attempt exponential growth factor
	// of the base delay before jitter.
	reconnectBackoffFactor = 2.0
)

// DefaultReconcileTimeout bounds each broker SUBSCRIBE / UNSUBSCRIBE issued
// during SessionPlan reconciliation when reconcile_timeout is unset or
// non-positive (HIGH-2). 30s comfortably covers a healthy broker's SUBACK /
// UNSUBACK round-trip while ensuring a wedged broker cannot hang the reconcile
// (and any startup / hot-reload step awaiting it) indefinitely. It is a
// liveness safety bound, so a non-positive configured value is coerced UP to
// this default rather than disabling the timeout.
const DefaultReconcileTimeout = 30 * time.Second

// DefaultSessionOptions returns SessionOptions with recommended defaults.
//
// CleanStart defaults to FALSE: only Persistent/Exclusive sessions
// consult the flag (Ephemeral always connects clean), and those modes
// exist to resume broker session state.
func DefaultSessionOptions() SessionOptions {
	return SessionOptions{
		KeepAlive:        30,
		ConnectTimeout:   DefaultConnectTimeout,
		ReconnectTimeout: 30 * time.Second,
		ReconcileTimeout: DefaultReconcileTimeout,
		CleanStart:       false,
		UnmatchedGrace:   DefaultUnmatchedGrace,
		Clock:            clock.System,
	}
}

// DefaultSenderOptions returns SenderOptions with recommended defaults.
func DefaultSenderOptions() SenderOptions {
	return SenderOptions{
		QoS:                1,
		Timeout:            30 * time.Second,
		ThrottleRetryAfter: 500 * time.Millisecond,
	}
}

// DefaultConfig returns a Config pre-filled with documented defaults except the
// three ingress-memory safety fields. Those remain zero ("unset") through
// parse/clone/bootstrap so an omitted value cannot become indistinguishable from
// an explicit operator value before the AWS profile derives its allocation.
// The registry decoder (register.go) decodes INTO this value:
// mapstructure only assigns fields present in the input map, so an
// omitted key keeps its default while an explicit value — including an
// explicit zero such as `qos: 0` or `keep_alive: 0` — overrides it.
// This is what makes "unset ⇒ default, explicit 0 ⇒ honoured"
// distinguishable without pointer-typed config fields.
func DefaultConfig() Config {
	session := DefaultSessionOptions()
	// Clock is an injected dependency, never config state; keep the
	// decoded Config free of it (NewSession installs clock.System).
	session.Clock = nil
	return Config{
		Session: session,
		Sender:  DefaultSenderOptions(),
	}
}

// SessionOptionsFromMap extracts SessionOptions from a generic options map.
// It returns an error if a provided value has an invalid type or range.
func SessionOptionsFromMap(m map[string]any) (SessionOptions, error) {
	opts := DefaultSessionOptions()
	if m == nil {
		return opts.normalizedIngressMemory(), nil
	}

	switch v := m["broker_urls"].(type) {
	case []string:
		opts.BrokerURLs = v
	case []any:
		urls := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				urls = append(urls, s)
			}
		}
		if len(urls) > 0 {
			opts.BrokerURLs = urls
		}
	}
	if v, ok := m["broker_url"].(string); ok && len(opts.BrokerURLs) == 0 {
		opts.BrokerURLs = []string{v}
	}
	if v, ok := m["client_id"].(string); ok {
		opts.ClientID = v
	}
	if raw, exists := m["keep_alive"]; exists {
		var v int
		switch n := raw.(type) {
		case int:
			v = n
		case int64:
			v = int(n)
		case float64:
			v = int(n)
		default:
			return opts, fmt.Errorf("keep_alive must be a number, got %T", raw)
		}
		if v < 0 || v > 65535 {
			return opts, fmt.Errorf("keep_alive must be 0..65535, got %d", v)
		}
		opts.KeepAlive = uint16(v)
	}
	if v, ok := optDuration(m, "connect_timeout"); ok {
		opts.ConnectTimeout = v
	}
	if v, ok := optDuration(m, "reconnect_timeout"); ok {
		opts.ReconnectTimeout = v
	}
	if v, ok := optDuration(m, "reconcile_timeout"); ok {
		opts.ReconcileTimeout = v
	}
	if v, ok := optDuration(m, "reconnect_delay"); ok {
		opts.ReconnectDelay = v
	}
	if v, ok := optDuration(m, "reconnect_max_delay"); ok {
		opts.ReconnectMaxDelay = v
	}
	if v, ok := optDuration(m, "unmatched_grace"); ok {
		opts.UnmatchedGrace = v
	}
	if v, ok := m["clean_start"].(bool); ok {
		opts.CleanStart = v
	}
	if v, ok := m["no_local"].(bool); ok {
		opts.NoLocal = v
	}
	if raw, exists := m["session_expiry_interval"]; exists {
		// MQTT v5 SessionExpiryInterval is an unsigned 32-bit value
		// (seconds; 0xFFFFFFFF = "never expire"). Reject negative
		// inputs so a stray -1 is not silently coerced into "never
		// expire" via two's-complement wrap-around.
		var v int64
		switch n := raw.(type) {
		case int:
			v = int64(n)
		case int64:
			v = n
		case uint32:
			v = int64(n)
		case float64:
			if math.IsNaN(n) || math.IsInf(n, 0) {
				return opts, fmt.Errorf("session_expiry_interval must be a finite number, got %v", n)
			}
			v = int64(n)
		default:
			return opts, fmt.Errorf("session_expiry_interval must be a number, got %T", raw)
		}
		if v < 0 {
			return opts, fmt.Errorf("session_expiry_interval must be ≥ 0, got %d", v)
		}
		if v > math.MaxUint32 {
			return opts, fmt.Errorf("session_expiry_interval must be ≤ %d, got %d", uint32(math.MaxUint32), v)
		}
		opts.SessionExpiryInterval = uint32(v)
	}
	if value, exists, err := optUint64(m, "receive_maximum", math.MaxUint16); err != nil {
		return opts, err
	} else if exists {
		opts.ReceiveMaximum = uint16(value)
		opts.receiveMaximumExplicit = value != 0
	}
	if value, exists, err := optUint64(m, "max_payload_bytes", math.MaxUint32); err != nil {
		return opts, err
	} else if exists {
		opts.MaxPayloadBytes = uint32(value)
	}
	if value, exists, err := optUint64(m, "ingress_memory_budget_bytes", math.MaxUint64); err != nil {
		return opts, err
	} else if exists {
		opts.IngressMemoryBudgetBytes = value
		opts.ingressMemoryBudgetExplicit = value != 0
	}
	if v, ok := m["username"].(string); ok {
		opts.Username = v
	}
	if v, ok := m["password"].(string); ok {
		opts.Password = shared.NewSecret(v)
	}
	if v, ok := m["allow_plaintext_credentials"].(bool); ok {
		opts.AllowPlaintextCredentials = v
	}
	if v, ok := m["tls"].(*TLSConfig); ok {
		opts.TLS = v
	}
	if v, ok := m["tls"].(map[string]any); ok {
		opts.TLS = tlsConfigFromMap(v)
	}
	if v, ok := m["will"].(*WillOptions); ok {
		opts.Will = v
	}
	if v, ok := m["will"].(map[string]any); ok {
		will, err := willOptionsFromMap(v)
		if err != nil {
			return opts, err
		}
		opts.Will = will
	}

	// HIGH-4: reject cleartext username/password over a non-TLS broker unless
	// explicitly opted in. Guarded internally by broker_urls being present.
	if err := opts.validatePlaintextCredentials(); err != nil {
		return opts, err
	}

	return opts.normalizedIngressMemory(), nil
}

// willOptionsFromMap extracts WillOptions from a generic options map.
func willOptionsFromMap(m map[string]any) (*WillOptions, error) {
	w := &WillOptions{}
	if v, ok := m["topic"].(string); ok {
		w.Topic = v
	}
	if v, ok := m["payload"].(string); ok {
		w.Payload = v
	}
	if raw, exists := m["qos"]; exists {
		var v int
		switch n := raw.(type) {
		case int:
			v = n
		case int64:
			v = int(n)
		case float64:
			v = int(n)
		default:
			return nil, fmt.Errorf("will.qos must be a number, got %T", raw)
		}
		if v < 0 || v > 2 {
			return nil, fmt.Errorf("will.qos must be 0, 1, or 2, got %d", v)
		}
		w.QoS = byte(v)
	}
	if v, ok := m["retain"].(bool); ok {
		w.Retain = v
	}
	if err := w.Validate(); err != nil {
		return nil, err
	}
	return w, nil
}

// SenderOptionsFromMap extracts SenderOptions from a generic options map.
// It returns an error if a provided value has an invalid type or range.
func SenderOptionsFromMap(m map[string]any) (SenderOptions, error) {
	opts := DefaultSenderOptions()
	if m == nil {
		return opts, nil
	}

	if v, ok := m["default_topic"].(string); ok {
		opts.DefaultTopic = v
	}
	if raw, exists := m["qos"]; exists {
		var v int
		switch n := raw.(type) {
		case int:
			v = n
		case int64:
			v = int(n)
		case float64:
			v = int(n)
		default:
			return opts, fmt.Errorf("qos must be a number, got %T", raw)
		}
		if v < 0 || v > 2 {
			return opts, fmt.Errorf("qos must be 0, 1, or 2, got %d", v)
		}
		opts.QoS = byte(v)
	}
	if v, ok := m["retain"].(bool); ok {
		opts.Retain = v
	}
	if v, ok := optDuration(m, "timeout"); ok {
		opts.Timeout = v
	}
	if v, ok := optDuration(m, "throttle_retry_after"); ok {
		opts.ThrottleRetryAfter = v
	}

	return opts, nil
}

// ReceiverOptionsFromMap extracts ReceiverOptions from a generic options map.
func ReceiverOptionsFromMap(m map[string]any) ReceiverOptions {
	return ReceiverOptions{}
}

func tlsConfigFromMap(m map[string]any) *TLSConfig {
	cfg := &TLSConfig{}
	if v, ok := m["enable"].(bool); ok {
		cfg.Enable = v
	}
	if v, ok := m["ca_cert_file"].(string); ok {
		cfg.CACertFile = v
	}
	if v, ok := m["cert_file"].(string); ok {
		cfg.CertFile = v
	}
	if v, ok := m["key_file"].(string); ok {
		cfg.KeyFile = v
	}
	// In-memory PEM material (ca_cert_pem / cert_pem / key_pem). The typed
	// decode path and BuildTLSConfig fully support these and let them WIN over
	// the *_file keys, but this map path previously ignored them: a library
	// consumer passing PEM through the map silently got system roots and no
	// client certificate — an opaque auth failure (A-8). Parse them here so both
	// config paths behave identically. They are shared.Secret so key material
	// redacts on marshal/log.
	if v, ok := m["ca_cert_pem"].(string); ok && v != "" {
		cfg.CACertPEM = shared.NewSecret(v)
	}
	if v, ok := m["cert_pem"].(string); ok && v != "" {
		cfg.CertPEM = shared.NewSecret(v)
	}
	if v, ok := m["key_pem"].(string); ok && v != "" {
		cfg.KeyPEM = shared.NewSecret(v)
	}
	if v, ok := m["insecure_skip_verify"].(bool); ok {
		cfg.InsecureSkipVerify = v
	}
	return cfg
}

// BuildTLSConfig creates a *tls.Config from TLSConfig.
//
// Material source dispatch:
//   - CA: CACertPEM wins over CACertFile when non-empty.
//   - Client cert/key: both CertPEM+KeyPEM present win over CertFile+KeyFile.
//     Partial PEM pairs (only one of CertPEM/KeyPEM) are rejected to avoid
//     a silent "loaded the file pair" fallback that hides a rotation bug.
func BuildTLSConfig(cfg *TLSConfig) (*tls.Config, error) {
	if cfg == nil {
		return nil, nil
	}

	tlsCfg := &tls.Config{
		// Explicit floor (Go's client default is already TLS 1.2, so this
		// changes no accepted-version behaviour) — a documented minimum is
		// cheaper defence-in-depth than relying on the library default (L-3).
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // caller-controlled
	}

	switch {
	case !cfg.CACertPEM.IsZero():
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(cfg.CACertPEM.Reveal())) {
			return nil, fmt.Errorf("failed to parse CA cert PEM material")
		}
		tlsCfg.RootCAs = pool
	case cfg.CACertFile != "":
		caCert, err := os.ReadFile(cfg.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("read CA cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA cert from %s", cfg.CACertFile)
		}
		tlsCfg.RootCAs = pool
	}

	switch {
	case !cfg.CertPEM.IsZero() && !cfg.KeyPEM.IsZero():
		cert, err := tls.X509KeyPair([]byte(cfg.CertPEM.Reveal()), []byte(cfg.KeyPEM.Reveal()))
		if err != nil {
			return nil, fmt.Errorf("parse client certificate PEM: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	case !cfg.CertPEM.IsZero() || !cfg.KeyPEM.IsZero():
		return nil, fmt.Errorf("client certificate PEM requires both CertPEM and KeyPEM")
	case cfg.CertFile != "" && cfg.KeyFile != "":
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	return tlsCfg, nil
}

// optDuration extracts a duration from a hand-built options map for the
// SessionOptionsFromMap library-consumer path. An invalid or negative value
// returns (0, false) — the caller keeps the field's default — rather than an
// error: this map path is intentionally lenient for programmatic callers, and
// strict validation of malformed input lives on the registry/YAML surface
// (register.go's decoder rejects unknown or unparseable keys). L-1: the two
// public config surfaces differ in strictness by design; this leniency is the
// documented behaviour of the map path, not an oversight.
func optDuration(m map[string]any, key string) (time.Duration, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}

	switch d := v.(type) {
	case time.Duration:
		if d < 0 {
			return 0, false
		}
		return d, true
	case string:
		parsed, err := time.ParseDuration(d)
		if err != nil || parsed < 0 {
			return 0, false
		}
		return parsed, true
	case int:
		if d < 0 {
			return 0, false
		}
		return time.Duration(d) * time.Second, true
	case int64:
		if d < 0 {
			return 0, false
		}
		return time.Duration(d) * time.Second, true
	case float64:
		if d < 0 || math.IsNaN(d) || math.IsInf(d, 0) {
			return 0, false
		}
		return time.Duration(d * float64(time.Second)), true
	default:
		return 0, false
	}
}

func optUint64(m map[string]any, key string, maximum uint64) (uint64, bool, error) {
	raw, exists := m[key]
	if !exists {
		return 0, false, nil
	}
	value, err := checkedConfigUint64(raw, key)
	if err != nil {
		return 0, false, err
	}
	if value > maximum {
		return 0, false, shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("%s must be ≤ %d, got %d", key, maximum, value),
		)
	}
	return value, true, nil
}

func checkedConfigUint64(raw any, key string) (uint64, error) {
	switch number := raw.(type) {
	case int:
		if number < 0 {
			return 0, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("%s must be non-negative, got %d", key, number),
			)
		}
		return uint64(number), nil
	case int8:
		if number < 0 {
			return 0, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("%s must be non-negative, got %d", key, number),
			)
		}
		return uint64(number), nil
	case int16:
		if number < 0 {
			return 0, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("%s must be non-negative, got %d", key, number),
			)
		}
		return uint64(number), nil
	case int32:
		if number < 0 {
			return 0, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("%s must be non-negative, got %d", key, number),
			)
		}
		return uint64(number), nil
	case int64:
		if number < 0 {
			return 0, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("%s must be non-negative, got %d", key, number),
			)
		}
		return uint64(number), nil
	case uint:
		return uint64(number), nil
	case uint8:
		return uint64(number), nil
	case uint16:
		return uint64(number), nil
	case uint32:
		return uint64(number), nil
	case uint64:
		return number, nil
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || math.Trunc(number) != number {
			return 0, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("%s must be a finite non-negative integer, got %v", key, number),
			)
		}
		// float64(math.MaxUint64) rounds UP to 2^64, so comparing against it
		// would admit 2^64 and wrap on conversion. Use the exact exclusive
		// power-of-two boundary instead.
		if number >= math.Ldexp(1, 64) {
			return 0, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("%s must be less than 2^64, got %v", key, number),
			)
		}
		return uint64(number), nil
	default:
		return 0, shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("%s must be a number, got %T", key, raw),
		)
	}
}

// ClientIDSuffixHostname and ClientIDSuffixNonce are the supported
// client_id_suffix tokens (M-3).
const (
	ClientIDSuffixHostname = "hostname"
	ClientIDSuffixNonce    = "nonce"
)

// resolveClientIDSuffix returns base with the suffix token expanded and
// appended ("-<value>"), producing a per-replica-unique client_id for
// $share scale-out from a single shared config file (M-3). An empty suffix
// returns base unchanged. An unsupported token, or a failed hostname
// lookup, returns an error so the misconfiguration fails the build rather
// than silently colliding client_ids. Callers must enforce mode-specific
// suffix restrictions before calling this (see factory.NewSession).
type clientIDSuffixProcessIdentity struct {
	hostnameOnce sync.Once
	hostname     string
	hostnameErr  error
	nonceOnce    sync.Once
	nonce        string
	nonceErr     error
	random       io.Reader
}

// processClientIDSuffixIdentity intentionally has process scope: independently
// decoded reload configs must resolve the same effective ID. Config value copies
// retain an explicit state pointer when one is supplied.
var processClientIDSuffixIdentity = clientIDSuffixProcessIdentity{random: rand.Reader} //nolint:gochecknoglobals

func (c Config) suffixIdentity() *clientIDSuffixProcessIdentity {
	if c.clientIDSuffixIdentity != nil {
		return c.clientIDSuffixIdentity
	}
	return &processClientIDSuffixIdentity
}

func (c Config) resolveClientIDSuffix(base, suffix string) (string, error) {
	return c.suffixIdentity().resolve(base, suffix)
}

// resolveClientIDSuffix preserves the package-level helper for callers that do
// not have a Config. Config capabilities and the factory use the state carried
// by Config so copied values cannot bypass suffix stability.
func resolveClientIDSuffix(base, suffix string) (string, error) {
	return processClientIDSuffixIdentity.resolve(base, suffix)
}

func (identity *clientIDSuffixProcessIdentity) resolve(base, suffix string) (string, error) {
	switch suffix {
	case "":
		return base, nil
	case ClientIDSuffixHostname:
		identity.hostnameOnce.Do(func() {
			identity.hostname, identity.hostnameErr = os.Hostname()
			if identity.hostnameErr == nil && identity.hostname == "" {
				identity.hostnameErr = errors.New("hostname is empty")
			}
		})
		if identity.hostnameErr != nil {
			return "", fmt.Errorf("client_id_suffix %q: hostname lookup failed: %w", suffix, identity.hostnameErr)
		}
		return base + "-" + identity.hostname, nil
	case ClientIDSuffixNonce:
		identity.nonceOnce.Do(func() {
			random := identity.random
			if random == nil {
				random = rand.Reader
			}
			identity.nonce, identity.nonceErr = randomClientNonce(random)
		})
		if identity.nonceErr != nil {
			return "", fmt.Errorf("client_id_suffix %q: nonce entropy failed: %w", suffix, identity.nonceErr)
		}
		return base + "-" + identity.nonce, nil
	default:
		return "", fmt.Errorf("client_id_suffix %q: unsupported (want %q or %q)",
			suffix, ClientIDSuffixHostname, ClientIDSuffixNonce)
	}
}

// randomClientNonce returns 128 bits of cryptographic entropy as hex. Entropy
// failure is returned to the caller; silently deriving a weaker fallback could
// collide replica client IDs and cause broker mutual-takeover storms.
func randomClientNonce(random io.Reader) (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(random, b); err != nil {
		return "", fmt.Errorf("read nonce entropy: %w", err)
	}
	return hex.EncodeToString(b), nil
}
