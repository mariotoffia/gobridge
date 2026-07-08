package paho

import (
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// MINOR: password sent only when username non-empty.
//
// The old ConnectPacketBuilder gated the password on a non-empty username, so a
// password-WITHOUT-username (a common token/JWT shape where the bearer token
// rides in the password with no username) was silently dropped. MQTT v5
// [MQTT-3.1.2-16..21] permits Password Flag = 1 with Username Flag = 0.
//
// Fix: applyConnectCredentials drives each flag solely by whether its own value
// is present.
// ═══════════════════════════════════════════════════════════════════════════

func TestApplyConnectCredentials(t *testing.T) {
	t.Run("password_without_username", func(t *testing.T) {
		cp := &pahov5.Connect{}
		applyConnectCredentials(cp, "", "bearer-token")

		require.False(t, cp.UsernameFlag, "empty username ⇒ Username Flag stays 0")
		require.Empty(t, cp.Username)
		require.True(t, cp.PasswordFlag,
			"password present ⇒ Password Flag set even WITHOUT a username (MQTT v5 token auth)")
		require.Equal(t, []byte("bearer-token"), cp.Password)
	})

	t.Run("username_and_password", func(t *testing.T) {
		cp := &pahov5.Connect{}
		applyConnectCredentials(cp, "user", "pass")

		require.True(t, cp.UsernameFlag)
		require.Equal(t, "user", cp.Username)
		require.True(t, cp.PasswordFlag)
		require.Equal(t, []byte("pass"), cp.Password)
	})

	t.Run("username_without_password", func(t *testing.T) {
		cp := &pahov5.Connect{}
		applyConnectCredentials(cp, "user", "")

		require.True(t, cp.UsernameFlag)
		require.Equal(t, "user", cp.Username)
		require.False(t, cp.PasswordFlag, "empty password ⇒ Password Flag stays 0")
		require.Nil(t, cp.Password)
	})

	t.Run("neither", func(t *testing.T) {
		cp := &pahov5.Connect{}
		applyConnectCredentials(cp, "", "")

		require.False(t, cp.UsernameFlag)
		require.False(t, cp.PasswordFlag)
		require.Empty(t, cp.Username)
		require.Nil(t, cp.Password)
	})
}
