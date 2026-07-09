package file

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// writeRawCredFile plants a raw credentials file on disk, bypassing Create so a
// load-path test can exercise a torn/blanked file the write path would reject.
func writeRawCredFile(t *testing.T, repo *Repository, uri, body string) {
	t.Helper()
	fp, err := repo.uriToPath(uri)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(fp), 0o700))
	require.NoError(t, os.WriteFile(fp, []byte(body), 0o600))
}

// TestGet_EmptyCredentialSetRejected is the c12 load-path regression: a
// credential set present on disk but carrying NO usable material must be
// rejected as an invalid payload, never resolved to CredentialSet{nil,nil}
// (which would let a transport connect with no auth). Emptiness is judged after
// trimming, so whitespace-only material counts as absent. This is the
// auth-stripping hole the finding targets and it must stay closed even after
// the acceptance space was widened to username-or-password.
//
// Mutation killed: dropping the ensureUsableCredential call in Get accepts the
// fully-empty set and this test FAILs (Get returns a non-nil, non-error set).
func TestGet_EmptyCredentialSetRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"fully empty set", `{"credentials":{},"version":1}`},
		{"empty username and password", `{"credentials":{"Password":{"Username":"","Password":""}},"version":1}`},
		{"whitespace username and password", `{"credentials":{"Password":{"Username":"  ","Password":"\t"}},"version":1}`},
		{"whitespace-only TLS material", `{"credentials":{"TLS":{"CertPEM":" ","KeyPEM":"\n","CAPEMs":[""]}},"version":1}`},
		{"whitespace-only CA bundle", `{"credentials":{"TLS":{"CAPEMs":["   "]}},"version":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, err := New(t.TempDir())
			require.NoError(t, err)

			uri := "file://broker"
			writeRawCredFile(t, repo, uri, tc.body)

			creds, err := repo.Get(context.Background(), uri)
			require.Nil(t, creds, "an empty credential set must never resolve to a usable value")
			require.Error(t, err, "an empty credential set must be rejected, not returned")
			require.ErrorIs(t, err, shared.ErrInvalidPayload)
		})
	}
}

// TestGet_TornTLSHalfRejected pins the torn-TLS correction: a lone cert or a
// lone key is rejected at load time UNCONDITIONALLY — including when usable
// basic auth is present — so it surfaces here rather than as a confusing
// connect-time "requires both" failure in the transport.
//
// Mutation killed: making the torn check conditional on the absence of basic
// auth (or removing it) lets the "…, WITH password" cases through and this
// test FAILs.
func TestGet_TornTLSHalfRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"cert without key, no basic auth", `{"credentials":{"TLS":{"CertPEM":"cert","KeyPEM":""}},"version":1}`},
		{"key without cert, no basic auth", `{"credentials":{"TLS":{"KeyPEM":"key"}},"version":1}`},
		{"cert without key, WITH password", `{"credentials":{"Password":{"Username":"u","Password":"p"},"TLS":{"CertPEM":"cert"}},"version":1}`},
		{"key without cert, WITH password", `{"credentials":{"Password":{"Username":"u","Password":"p"},"TLS":{"KeyPEM":"key"}},"version":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, err := New(t.TempDir())
			require.NoError(t, err)

			uri := "file://broker"
			writeRawCredFile(t, repo, uri, tc.body)

			creds, err := repo.Get(context.Background(), uri)
			require.Nil(t, creds)
			require.Error(t, err)
			require.ErrorIs(t, err, shared.ErrInvalidPayload)
			assert.Contains(t, err.Error(), "certificate and key must both be present")
		})
	}
}

// TestGet_ValidCredentialSetAccepted pins the widened acceptance space: a set
// carrying at least one usable credential still loads. Username-only (empty
// password) is the regression proof — it loaded and connected before the c12
// guard existed and must keep loading.
func TestGet_ValidCredentialSetAccepted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		body         string
		wantPassword bool
		wantTLS      bool
	}{
		{"username and password", `{"credentials":{"Password":{"Username":"u","Password":"p"}},"version":1}`, true, false},
		{"username only (empty password)", `{"credentials":{"Password":{"Username":"u","Password":""}},"version":1}`, true, false},
		{"password only (empty username)", `{"credentials":{"Password":{"Username":"","Password":"p"}},"version":1}`, true, false},
		{"TLS CA bundle only", `{"credentials":{"TLS":{"CAPEMs":["ca-bundle-pem"]}},"version":1}`, false, true},
		{"TLS complete cert+key pair", `{"credentials":{"TLS":{"CertPEM":"cert","KeyPEM":"key"}},"version":1}`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, err := New(t.TempDir())
			require.NoError(t, err)

			uri := "file://broker"
			writeRawCredFile(t, repo, uri, tc.body)

			creds, err := repo.Get(context.Background(), uri)
			require.NoError(t, err)
			require.NotNil(t, creds)
			assert.Equal(t, tc.wantPassword, creds.Password() != nil, "password material presence")
			assert.Equal(t, tc.wantTLS, creds.TLS() != nil, "TLS material presence")
		})
	}
}

// TestBasicAuthShapes_CreateUpdateGet is the regression proof for the
// username-or-password gate: username-only, password-only, and full basic-auth
// credentials all round-trip through Create, Get, and Update. Before the
// correction the guard keyed usability on the PASSWORD alone, so a legitimate
// username-only credential was rejected on load and every write.
//
// Mutation killed: reverting hasUsableBasicAuth to a password-only gate
// (strings.TrimSpace(dto.Password.Password) != "") rejects the username-only
// case and this test FAILs on Create.
func TestBasicAuthShapes_CreateUpdateGet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		create     *connectivity.CredentialSet
		createUser string
		createPass string
		update     *connectivity.CredentialSet
		updateUser string
		updatePass string
	}{
		{"username only", passwordCreds("mquser", ""), "mquser", "", passwordCreds("mquser2", ""), "mquser2", ""},
		{"password only", passwordCreds("", "pw1"), "", "pw1", passwordCreds("", "pw2"), "", "pw2"},
		{"username and password", passwordCreds("u", "p"), "u", "p", passwordCreds("u2", "p2"), "u2", "p2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, err := New(t.TempDir())
			require.NoError(t, err)
			ctx := context.Background()

			uri := "file://broker"
			require.NoError(t, repo.Create(ctx, uri, tc.create), "Create must accept a username-or-password credential")

			got, err := repo.Get(ctx, uri)
			require.NoError(t, err)
			require.NotNil(t, got.Password())
			assert.Equal(t, tc.createUser, got.Password().Username())
			assert.Equal(t, tc.createPass, got.Password().Password().Reveal())

			require.NoError(t, repo.Update(ctx, uri, tc.update, 0), "Update must accept a username-or-password credential")

			got, err = repo.Get(ctx, uri)
			require.NoError(t, err)
			require.NotNil(t, got.Password())
			assert.Equal(t, tc.updateUser, got.Password().Username())
			assert.Equal(t, tc.updatePass, got.Password().Password().Reveal())
		})
	}
}

// TestCreate_EmptyCredentialSetRejected verifies the write path never persists
// an empty set: Create must reject it and leave no file behind. Covers both the
// fully-nil set and a present-but-blank basic-auth entry.
func TestCreate_EmptyCredentialSetRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		creds *connectivity.CredentialSet
	}{
		{"fully empty set", connectivity.NewCredentialSet(nil, nil)},
		{"empty username and password", passwordCreds("", "")},
		{"whitespace username and password", passwordCreds("  ", "\t")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, err := New(t.TempDir())
			require.NoError(t, err)
			ctx := context.Background()

			uri := "file://broker"
			err = repo.Create(ctx, uri, tc.creds)
			require.Error(t, err)
			require.ErrorIs(t, err, shared.ErrInvalidPayload)

			fp, err := repo.uriToPath(uri)
			require.NoError(t, err)
			_, statErr := os.Stat(fp)
			assert.True(t, os.IsNotExist(statErr), "no credentials file may be written for an empty set")
		})
	}
}

// TestCreate_TornTLSHalfRejected verifies the write path also rejects a torn
// TLS pair even when basic auth is present, and still accepts a complete pair.
func TestCreate_TornTLSHalfRejected(t *testing.T) {
	t.Parallel()

	pw := connectivity.NewPasswordCredential("u", "p")
	certOnly := connectivity.NewTLSMaterial("cert", "", nil, false)
	keyOnly := connectivity.NewTLSMaterial("", "key", nil, false)
	completePair := connectivity.NewTLSMaterial("cert", "key", nil, false)

	repo, err := New(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()

	err = repo.Create(ctx, "file://cert-only", connectivity.NewCredentialSet(&pw, &certOnly))
	require.ErrorIs(t, err, shared.ErrInvalidPayload)

	err = repo.Create(ctx, "file://key-only", connectivity.NewCredentialSet(&pw, &keyOnly))
	require.ErrorIs(t, err, shared.ErrInvalidPayload)

	err = repo.Create(ctx, "file://complete", connectivity.NewCredentialSet(&pw, &completePair))
	require.NoError(t, err, "a complete cert+key pair (with basic auth) must be accepted")
}

// TestUpdate_RotationToEmptyRejected is the c12 rotation regression: rotating a
// live, valid credential to an empty (or whitespace-only) set must be rejected
// so it can never silently strip auth material. The originally stored
// credential must remain intact and readable afterwards.
//
// Mutation killed: dropping the ensureUsableCredential call in Update accepts
// the empty rotation and this test FAILs (the stored credential is replaced by
// nothing / Get no longer returns the original).
func TestUpdate_RotationToEmptyRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		creds *connectivity.CredentialSet
	}{
		{"fully empty set", connectivity.NewCredentialSet(nil, nil)},
		{"empty username and password", passwordCreds("", "")},
		{"whitespace username and password", passwordCreds("  ", "  ")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, err := New(t.TempDir())
			require.NoError(t, err)
			ctx := context.Background()

			uri := "file://broker"
			require.NoError(t, repo.Create(ctx, uri, passwordCreds("live-user", "live-pass")))

			err = repo.Update(ctx, uri, tc.creds, 0)
			require.Error(t, err, "a rotation to an empty set must be rejected")
			require.ErrorIs(t, err, shared.ErrInvalidPayload)

			// The live credential must survive the rejected rotation.
			got, err := repo.Get(ctx, uri)
			require.NoError(t, err)
			require.NotNil(t, got.Password())
			assert.Equal(t, "live-user", got.Password().Username())
			assert.Equal(t, "live-pass", got.Password().Password().Reveal())
		})
	}
}

// TestUpdate_RotationToTornTLSRejected closes the coverage-symmetry gap: the
// shared ensureUsableCredential gate must reject a torn TLS half on the
// Update/rotation path too (Get and Create are already covered), and — like
// every rejected rotation — must leave the live stored credential untouched.
// Each rotation carries a valid password alongside the torn half, proving the
// torn check fires unconditionally rather than only when basic auth is absent.
//
// Mutation killed: gating the torn check on the Create/Get paths only (e.g.
// skipping it in Update, or making it conditional on !hasUsableBasicAuth())
// lets the rotation through and this test FAILs (Update succeeds and clobbers
// the live credential with a torn half).
func TestUpdate_RotationToTornTLSRejected(t *testing.T) {
	t.Parallel()

	pw := connectivity.NewPasswordCredential("rot-user", "rot-pass")
	certOnly := connectivity.NewTLSMaterial("cert", "", nil, false)
	keyOnly := connectivity.NewTLSMaterial("", "key", nil, false)

	cases := []struct {
		name  string
		creds *connectivity.CredentialSet
	}{
		{"cert without key, WITH password", connectivity.NewCredentialSet(&pw, &certOnly)},
		{"key without cert, WITH password", connectivity.NewCredentialSet(&pw, &keyOnly)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, err := New(t.TempDir())
			require.NoError(t, err)
			ctx := context.Background()

			uri := "file://broker"
			require.NoError(t, repo.Create(ctx, uri, passwordCreds("live-user", "live-pass")))

			err = repo.Update(ctx, uri, tc.creds, 0)
			require.Error(t, err, "a rotation to a torn TLS half must be rejected")
			require.ErrorIs(t, err, shared.ErrInvalidPayload)
			assert.Contains(t, err.Error(), "certificate and key must both be present")

			// The live credential must survive the rejected rotation.
			got, err := repo.Get(ctx, uri)
			require.NoError(t, err)
			require.NotNil(t, got.Password())
			assert.Equal(t, "live-user", got.Password().Username())
			assert.Equal(t, "live-pass", got.Password().Password().Reveal())
			assert.Nil(t, got.TLS(), "no TLS material may have been written by the rejected rotation")
		})
	}
}

// TestUriToPath_ParseErrorRedactsEmbeddedCredential is the file-uri-leak
// regression: a URI that fails url.Parse while embedding a secret in its
// userinfo must not echo that secret into the returned error — only the
// redacted form (userinfo stripped) may appear.
//
// Mutation killed: dropping shared.RedactURIError in uriToPath echoes the raw
// *url.Error (which embeds the userinfo) and this test FAILs.
func TestUriToPath_ParseErrorRedactsEmbeddedCredential(t *testing.T) {
	t.Parallel()

	repo, err := New(t.TempDir())
	require.NoError(t, err)

	const secret = "SUPERSECRETVALUE"
	// Invalid port ":notaport" makes url.Parse fail; the *url.Error it returns
	// echoes the whole raw URI, including the user:secret@ userinfo.
	badURI := "file://user:" + secret + "@host:notaport/creds"

	_, err = repo.uriToPath(badURI)
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret,
		"the URI parse error must not echo the embedded credential")
	assert.Contains(t, err.Error(), "host:notaport",
		"only the redacted form (userinfo stripped) may appear")
}

// TestGet_ParseErrorNeverLeaksURICredential asserts the redaction holds on the
// public Get path too: neither the returned error nor any emitted log line may
// contain the embedded secret.
func TestGet_ParseErrorNeverLeaksURICredential(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	repo, err := New(t.TempDir(), WithLogger(logger))
	require.NoError(t, err)

	const secret = "SUPERSECRETVALUE"
	badURI := "file://user:" + secret + "@host:notaport/creds"

	_, err = repo.Get(context.Background(), badURI)
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret, "the returned error must not expose the secret")
	require.NotContains(t, logs.String(), secret, "no log line may contain the secret")
}
