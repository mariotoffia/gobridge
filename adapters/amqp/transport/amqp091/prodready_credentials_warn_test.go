// ═══════════════════════════════════════════════
// Production-readiness remediation tests: broker_url userinfo vs
// managed credentials (F8 residual).
//
// injectCredentials already makes explicit/rotated credentials WIN over
// userinfo embedded in broker_url (F8 core fix). The residual gap: the
// conflict itself was silent. An operator who embeds guest:guest in the
// URL *and* wires a credential store gets the managed credentials on the
// wire — but nothing tells them the URL userinfo is dead config that
// will mislead the next reader (and may be a stale secret sitting in
// YAML). The session must Warn when both are present: once at
// construction and once per credential rotation. The Warn must carry
// only the REDACTED broker URL.
//
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// warnRecordHandler records Warn-level messages (message + rendered
// attrs) for content assertions. Safe for concurrent use.
type warnRecordHandler struct {
	count atomic.Int64
	// joined accumulates "msg|attr=val" lines; guarded by the atomic
	// count ordering only — tests read it after all writes completed.
	messages []string
}

func (h *warnRecordHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

func (h *warnRecordHandler) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		sb.WriteString(" ")
		sb.WriteString(a.Key)
		sb.WriteString("=")
		sb.WriteString(a.Value.String())
		return true
	})
	h.messages = append(h.messages, sb.String())
	h.count.Add(1)
	return nil
}

func (h *warnRecordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *warnRecordHandler) WithGroup(string) slog.Handler      { return h }

func (h *warnRecordHandler) find(substr string) []string {
	var out []string
	for _, m := range h.messages {
		if strings.Contains(m, substr) {
			out = append(out, m)
		}
	}
	return out
}

// TestNewSession_WarnsWhenBrokerURLEmbedsUserinfoAndUsernameSet pins the
// construction-time conflict Warn, with the secret redacted.
func TestNewSession_WarnsWhenBrokerURLEmbedsUserinfoAndUsernameSet(t *testing.T) {
	h := &warnRecordHandler{}
	s := NewSession(SessionOptions{
		BrokerURL: "amqp://urluser:urlsecret@broker.example:5672/",
		Username:  "managed-user",
		Password:  shared.NewSecret("managed-secret"),
	}, connectivity.SessionEphemeral, slog.New(h))
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	warns := h.find("broker_url embeds credentials")
	require.Len(t, warns, 1,
		"conflicting URL userinfo + explicit username must Warn exactly once at construction")
	for _, w := range warns {
		require.NotContains(t, w, "urlsecret", "warn must not leak the embedded secret")
		require.NotContains(t, w, "managed-secret", "warn must not leak the managed secret")
	}
}

// TestNewSession_NoWarn_WhenOnlyURLUserinfo — URL userinfo alone is the
// legitimate credential source; no conflict, no Warn.
func TestNewSession_NoWarn_WhenOnlyURLUserinfo(t *testing.T) {
	h := &warnRecordHandler{}
	s := NewSession(SessionOptions{
		BrokerURL: "amqp://urluser:urlsecret@broker.example:5672/",
	}, connectivity.SessionEphemeral, slog.New(h))
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	require.Empty(t, h.find("broker_url embeds credentials"))
}

// TestNewSession_NoWarn_WhenOnlyExplicitUsername — explicit credentials
// with a clean URL is the recommended shape; no Warn.
func TestNewSession_NoWarn_WhenOnlyExplicitUsername(t *testing.T) {
	h := &warnRecordHandler{}
	s := NewSession(SessionOptions{
		BrokerURL: "amqp://broker.example:5672/",
		Username:  "managed-user",
		Password:  shared.NewSecret("managed-secret"),
	}, connectivity.SessionEphemeral, slog.New(h))
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	require.Empty(t, h.find("broker_url embeds credentials"))
}

// TestApplyCredentials_WarnsWhenBrokerURLEmbedsUserinfo pins the
// rotation-time Warn: rotation succeeds (rotated material wins on the
// next dial) but the operator is told the URL still embeds stale
// credentials.
func TestApplyCredentials_WarnsWhenBrokerURLEmbedsUserinfo(t *testing.T) {
	h := &warnRecordHandler{}
	s := NewSession(SessionOptions{
		BrokerURL: "amqp://urluser:urlsecret@broker.example:5672/",
	}, connectivity.SessionEphemeral, slog.New(h))
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	require.NoError(t, s.ApplyCredentials(t.Context(),
		connectivity.NewCredentialSet(pwCred("rotated-user", "rotated-secret"), nil)))

	warns := h.find("broker_url embeds credentials")
	require.NotEmpty(t, warns, "rotation over an embedded-userinfo URL must Warn")
	for _, w := range warns {
		require.NotContains(t, w, "urlsecret")
		require.NotContains(t, w, "rotated-secret")
	}

	// The core F8 behavior is unchanged: rotated credentials win.
	require.Contains(t, s.brokerURL(), "rotated-user:rotated-secret@",
		"rotated credentials must override URL userinfo on the next dial")
}
