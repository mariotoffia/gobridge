package ssm

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// ---------------------------------------------------------------------------
// Mock SSM client
// ---------------------------------------------------------------------------

type mockSSM struct {
	getParameterFn        func(ctx context.Context, input *awsssm.GetParameterInput) (*awsssm.GetParameterOutput, error)
	putParameterFn        func(ctx context.Context, input *awsssm.PutParameterInput) (*awsssm.PutParameterOutput, error)
	deleteParameterFn     func(ctx context.Context, input *awsssm.DeleteParameterInput) (*awsssm.DeleteParameterOutput, error)
	getParametersByPathFn func(ctx context.Context, input *awsssm.GetParametersByPathInput) (*awsssm.GetParametersByPathOutput, error)
}

func (m *mockSSM) GetParameter(ctx context.Context, input *awsssm.GetParameterInput, _ ...func(*awsssm.Options)) (*awsssm.GetParameterOutput, error) {
	if m.getParameterFn != nil {
		return m.getParameterFn(ctx, input)
	}
	return nil, fmt.Errorf("GetParameter not mocked")
}

func (m *mockSSM) PutParameter(ctx context.Context, input *awsssm.PutParameterInput, _ ...func(*awsssm.Options)) (*awsssm.PutParameterOutput, error) {
	if m.putParameterFn != nil {
		return m.putParameterFn(ctx, input)
	}
	return nil, fmt.Errorf("PutParameter not mocked")
}

func (m *mockSSM) DeleteParameter(ctx context.Context, input *awsssm.DeleteParameterInput, _ ...func(*awsssm.Options)) (*awsssm.DeleteParameterOutput, error) {
	if m.deleteParameterFn != nil {
		return m.deleteParameterFn(ctx, input)
	}
	return nil, fmt.Errorf("DeleteParameter not mocked")
}

func (m *mockSSM) GetParametersByPath(ctx context.Context, input *awsssm.GetParametersByPathInput, _ ...func(*awsssm.Options)) (*awsssm.GetParametersByPathOutput, error) {
	if m.getParametersByPathFn != nil {
		return m.getParametersByPathFn(ctx, input)
	}
	return nil, fmt.Errorf("GetParametersByPath not mocked")
}

var _ ssmAPI = (*mockSSM)(nil)

// ---------------------------------------------------------------------------
// Parser tests
// ---------------------------------------------------------------------------

// Verifies JSON credential payloads parse into username/password credential sets.
func TestParseCredentials_JSONUsernamePassword(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		username string
		password string
	}{
		{
			name:     "standard format",
			input:    `{"username":"alice","password":"secret123"}`,
			username: "alice",
			password: "secret123",
		},
		{
			name:     "with explicit type",
			input:    `{"type":"usernamePassword","username":"bob","password":"pass"}`,
			username: "bob",
			password: "pass",
		},
		{
			name:     "alternate field names",
			input:    `{"user":"charlie","pass":"mypass"}`,
			username: "charlie",
			password: "mypass",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			creds, err := parseCredentials(tt.input)
			require.NoError(t, err)
			require.NotNil(t, creds.Password())
			assert.Equal(t, tt.username, creds.Password().Username())
			assert.Equal(t, tt.password, creds.Password().Password().Reveal())
			assert.Nil(t, creds.TLS())
		})
	}
}

// Verifies colon-separated user:password strings parse correctly.
func TestParseCredentials_SimpleFormat(t *testing.T) {
	creds, err := parseCredentials("myuser:mypassword")
	require.NoError(t, err)
	require.NotNil(t, creds.Password())
	assert.Equal(t, "myuser", creds.Password().Username())
	assert.Equal(t, "mypassword", creds.Password().Password().Reveal())
}

// Verifies simple format keeps colons in the password portion after the first separator.
func TestParseCredentials_SimpleFormat_PasswordWithColon(t *testing.T) {
	creds, err := parseCredentials("admin:pass:word:123")
	require.NoError(t, err)
	require.NotNil(t, creds.Password())
	assert.Equal(t, "admin", creds.Password().Username())
	assert.Equal(t, "pass:word:123", creds.Password().Password().Reveal())
}

// Verifies JSON TLS material parses into CertPEM, KeyPEM, CA PEMs, and insecure flag.
func TestParseCredentials_TLS(t *testing.T) {
	input := `{
		"certPem": "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----",
		"keyPem": "-----BEGIN PRIVATE KEY-----\nkey\n-----END PRIVATE KEY-----",
		"caPem": ["-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----"],
		"insecure": false
	}`

	creds, err := parseCredentials(input)
	require.NoError(t, err)
	require.NotNil(t, creds.TLS())
	assert.Nil(t, creds.Password())
	assert.NotEmpty(t, creds.TLS().CertPEM())
	assert.NotEmpty(t, creds.TLS().KeyPEM().Reveal())
	assert.Len(t, creds.TLS().CAPEMs(), 1)
	assert.False(t, creds.TLS().InsecureSkipVerify())
}

// Verifies TLS JSON sets InsecureSkipVerify when insecure is true.
func TestParseCredentials_TLS_InsecureSkipVerify(t *testing.T) {
	input := `{"certPem":"cert","keyPem":"key","insecure":true}`
	creds, err := parseCredentials(input)
	require.NoError(t, err)
	require.NotNil(t, creds.TLS())
	assert.True(t, creds.TLS().InsecureSkipVerify())
}

// Verifies a single ca string field is accepted as one CA PEM entry.
func TestParseCredentials_TLS_SingleCAPem(t *testing.T) {
	input := `{"certPem":"cert","keyPem":"key","ca":"single-ca"}`
	creds, err := parseCredentials(input)
	require.NoError(t, err)
	require.NotNil(t, creds.TLS())
	assert.Equal(t, []string{"single-ca"}, creds.TLS().CAPEMs())
}

// Verifies non-JSON, non-colon input returns an error from parseCredentials.
func TestParseCredentials_UnsupportedFormat(t *testing.T) {
	_, err := parseCredentials("no-colon-no-json")
	assert.Error(t, err)
}

// Verifies invalid JSON input returns an error from parseCredentials.
func TestParseCredentials_InvalidJSON(t *testing.T) {
	_, err := parseCredentials(`{invalid}`)
	assert.Error(t, err)
}

// Verifies JSON without recognizable credential fields returns an error.
func TestParseCredentials_UnknownJSONType(t *testing.T) {
	_, err := parseCredentials(`{"foo":"bar"}`)
	assert.Error(t, err)
}

// Verifies password-only JSON without a username returns an error.
func TestParseCredentials_MissingUsername(t *testing.T) {
	_, err := parseCredentials(`{"type":"password","password":"nouser"}`)
	assert.Error(t, err)
}

// Verifies TLS JSON with neither a CA bundle nor a complete cert/key pair
// is rejected rather than yielding a credential with empty trust material.
//
// Mutation reasoning: reverting parseTLSJSON to unconditionally return
// connectivity.NewTLSMaterial(...) (the pre-fix behaviour) makes every
// input below parse successfully — a failed/torn rotation write of
// `{"type":"tls"}` would silently strip TLS trust from a live transport.
// Each require.Error then fails.
func TestParseCredentials_EmptyTLSRejected(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"declared type only", `{"type":"tls"}`},
		{"empty cert and key strings", `{"certPem":"","keyPem":""}`},
		{"cert without key", `{"type":"tls","certPem":"only-cert"}`},
		{"key without cert", `{"type":"tls","keyPem":"only-key"}`},
		{"insecure only", `{"type":"tls","insecure":true}`},
		{"empty ca list", `{"type":"tls","caPem":[""]}`},
		{"whitespace ca entry", `{"type":"tls","caPem":[" "]}`},
		{"whitespace single ca", `{"type":"tls","ca":"  "}`},
		{"whitespace cert and key", `{"certPem":" ","keyPem":"\n"}`},
		{"whitespace cert without key", `{"type":"tls","certPem":"  "}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseCredentials(tc.input)
			require.Error(t, err)
		})
	}
}

// Verifies a CA-only credential (server verification, no mutual TLS) is
// accepted: the empty-material guard must not reject legitimate trust
// bundles carrying only a CA.
func TestParseCredentials_TLS_CAOnlyAccepted(t *testing.T) {
	creds, err := parseCredentials(`{"type":"tls","caPem":["root-ca"]}`)
	require.NoError(t, err)
	require.NotNil(t, creds.TLS())
	assert.Empty(t, creds.TLS().CertPEM())
	assert.Empty(t, creds.TLS().KeyPEM().Reveal())
	assert.Equal(t, []string{"root-ca"}, creds.TLS().CAPEMs())
}

// TestParseCredentials_TLS_CAOnlyNormalisesBlankCertKey proves a CA-only
// document whose cert/key fields are PRESENT but whitespace-only yields usable
// CA-only TLS material with EMPTY cert/key, never blank-but-non-empty strings
// that a transport would feed to tls.X509KeyPair and fail on at connect time.
//
// Mutation reasoning: dropping the whitespace-cert/key normalisation in
// parseTLSJSON stores " "/"\n" verbatim, so CertPEM()/KeyPEM() come back
// non-empty and the assert.Empty checks fail.
func TestParseCredentials_TLS_CAOnlyNormalisesBlankCertKey(t *testing.T) {
	creds, err := parseCredentials(`{"type":"tls","certPem":" ","keyPem":"\n","caPem":["root-ca"]}`)
	require.NoError(t, err)
	require.NotNil(t, creds.TLS())
	assert.Empty(t, creds.TLS().CertPEM(), "whitespace cert must normalise to empty")
	assert.Empty(t, creds.TLS().KeyPEM().Reveal(), "whitespace key must normalise to empty")
	assert.Equal(t, []string{"root-ca"}, creds.TLS().CAPEMs())
}

// TestParseCredentials_TLS_RealCertKeyPreserved proves a genuine cert+key pair
// is stored verbatim: the whitespace normalisation must touch only fields that
// are entirely blank, never real material.
func TestParseCredentials_TLS_RealCertKeyPreserved(t *testing.T) {
	creds, err := parseCredentials(`{"type":"tls","certPem":"cert-pem","keyPem":"key-pem"}`)
	require.NoError(t, err)
	require.NotNil(t, creds.TLS())
	assert.Equal(t, "cert-pem", creds.TLS().CertPEM())
	assert.Equal(t, "key-pem", creds.TLS().KeyPEM().Reveal())
}

// Verifies simple username:password strings with an empty username or
// password are rejected.
//
// Mutation reasoning: reverting parseSimpleCredentials to build a
// PasswordCredential from parts[0]/parts[1] without the emptiness guard
// makes ":pass", "user:" and ":" parse into anonymous/half-empty
// credentials, so the require.Error assertions fail.
func TestParseCredentials_EmptySimpleRejected(t *testing.T) {
	for _, input := range []string{":pass", "user:", ":"} {
		t.Run(input, func(t *testing.T) {
			_, err := parseCredentials(input)
			require.Error(t, err)
		})
	}
}

// ---------------------------------------------------------------------------
// Serialization round-trip
// ---------------------------------------------------------------------------

// Verifies serializeCredentialSet and parseCredentials round-trip password credentials.
func TestSerializeAndParseRoundTrip_Password(t *testing.T) {
	original := connectivity.NewCredentialSet(pwCred("u", "p"), nil)
	s, err := serializeCredentialSet(original)
	require.NoError(t, err)

	parsed, err := parseCredentials(s)
	require.NoError(t, err)
	require.NotNil(t, parsed.Password())
	assert.Equal(t, "u", parsed.Password().Username())
	assert.Equal(t, "p", parsed.Password().Password().Reveal())
}

// Verifies serializeCredentialSet and parseCredentials round-trip TLS material.
func TestSerializeAndParseRoundTrip_TLS(t *testing.T) {
	original := connectivity.NewCredentialSet(nil, tlsMat("cert-data", "key-data", []string{"ca1", "ca2"}, true))
	s, err := serializeCredentialSet(original)
	require.NoError(t, err)

	parsed, err := parseCredentials(s)
	require.NoError(t, err)
	require.NotNil(t, parsed.TLS())
	assert.Equal(t, "cert-data", parsed.TLS().CertPEM())
	assert.Equal(t, "key-data", parsed.TLS().KeyPEM().Reveal())
	assert.Equal(t, []string{"ca1", "ca2"}, parsed.TLS().CAPEMs())
	assert.True(t, parsed.TLS().InsecureSkipVerify())
}

// Verifies a credential set carrying BOTH password and TLS capabilities
// survives a serialize/parse round-trip intact. Regression test: the
// old parser dispatched on a single "type" and silently dropped all TLS
// material from a combined set, so mutual-TLS + SASL brokers regressed
// to TLS defaults after any Get.
func TestSerializeAndParseRoundTrip_PasswordAndTLS(t *testing.T) {
	original := connectivity.NewCredentialSet(
		pwCred("u", "p"),
		tlsMat("cert-data", "key-data", []string{"ca1", "ca2"}, true),
	)
	s, err := serializeCredentialSet(original)
	require.NoError(t, err)

	parsed, err := parseCredentials(s)
	require.NoError(t, err)

	require.NotNil(t, parsed.Password(), "password capability lost in round-trip")
	assert.Equal(t, "u", parsed.Password().Username())
	assert.Equal(t, "p", parsed.Password().Password().Reveal())

	require.NotNil(t, parsed.TLS(), "TLS capability lost in round-trip")
	assert.Equal(t, "cert-data", parsed.TLS().CertPEM())
	assert.Equal(t, "key-data", parsed.TLS().KeyPEM().Reveal())
	assert.Equal(t, []string{"ca1", "ca2"}, parsed.TLS().CAPEMs())
	assert.True(t, parsed.TLS().InsecureSkipVerify())
}

// Verifies hand-written JSON with coexisting password and TLS fields
// (no explicit combined "type") parses both capabilities.
func TestParseCredentials_JSONPasswordAndTLSFieldsCoexist(t *testing.T) {
	creds, err := parseCredentials(`{
		"username": "broker-user",
		"password": "broker-pass",
		"certPem": "cert-data",
		"keyPem": "key-data",
		"caPem": ["ca1"]
	}`)
	require.NoError(t, err)

	require.NotNil(t, creds.Password())
	assert.Equal(t, "broker-user", creds.Password().Username())
	assert.Equal(t, "broker-pass", creds.Password().Password().Reveal())

	require.NotNil(t, creds.TLS())
	assert.Equal(t, "cert-data", creds.TLS().CertPEM())
	assert.Equal(t, "key-data", creds.TLS().KeyPEM().Reveal())
	assert.Equal(t, []string{"ca1"}, creds.TLS().CAPEMs())
}

// Verifies the explicit combined type token drives both parsers and a
// declared-password capability still validates its mandatory fields.
func TestParseCredentials_CombinedTypeToken(t *testing.T) {
	creds, err := parseCredentials(`{
		"type": "password+tls",
		"username": "u",
		"password": "p",
		"certPem": "cert-data",
		"keyPem": "key-data"
	}`)
	require.NoError(t, err)
	require.NotNil(t, creds.Password())
	require.NotNil(t, creds.TLS())

	_, err = parseCredentials(`{"type": "password+tls", "certPem": "cert-data"}`)
	require.Error(t, err, "declared password capability without username must fail")
	assert.Contains(t, err.Error(), "missing username")
}

// ---------------------------------------------------------------------------
// URI parsing
// ---------------------------------------------------------------------------

// Verifies pms:// URIs map to SSM paths and invalid schemes error.
func TestParseURI(t *testing.T) {
	tests := []struct {
		name     string
		uri      string
		expected string
		wantErr  bool
	}{
		{
			name:     "simple path",
			uri:      "pms://prod/db/password",
			expected: "/prod/db/password",
		},
		{
			name:     "nested path",
			uri:      "pms://app/service/credentials/main",
			expected: "/app/service/credentials/main",
		},
		{
			name:     "host only",
			uri:      "pms://single",
			expected: "/single",
		},
		{
			name:    "triple slash has no canonical host",
			uri:     "pms:///secret/path",
			wantErr: true,
		},
		{
			name:    "wrong scheme",
			uri:     "vault://secret/path",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, err := parseURI(tt.uri)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, path)
		})
	}
}

// Verifies pathToURI produces stable pms:// URIs for absolute paths.
func TestPathToURI(t *testing.T) {
	assert.Equal(t, "pms://prod/db/password", pathToURI("/prod/db/password"))
	assert.Equal(t, "pms://single", pathToURI("/single"))
}

// TestParseURI_ErrorRedactsUserinfo verifies a URI that fails to parse never
// echoes embedded userinfo (user:pass@) into the returned error (MINOR).
func TestParseURI_ErrorRedactsUserinfo(t *testing.T) {
	// Control character forces url.Parse to fail; the raw URL would otherwise
	// be echoed verbatim (with the secret) by the wrapped *url.Error.
	_, err := parseURI("pms://user:s3cr3t@ns/\x7fbad")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "s3cr3t", "userinfo must be redacted from the parse error")
}

// ---------------------------------------------------------------------------
// Scheme and Namespace
// ---------------------------------------------------------------------------

// Verifies default repository scheme is pms.
func TestRepository_Scheme(t *testing.T) {
	r := New()
	assert.Equal(t, "pms", r.Scheme())
}

// Verifies WithNamespace sets the repository namespace returned by Namespace().
func TestRepository_Namespace(t *testing.T) {
	r := New(WithNamespace("myapp/prod"))
	assert.Equal(t, "myapp/prod", r.Namespace())
}

// Verifies default repository has an empty namespace.
func TestRepository_Namespace_Empty(t *testing.T) {
	r := New()
	assert.Equal(t, "", r.Namespace())
}

// ---------------------------------------------------------------------------
// Repository CRUD (mock-based)
// ---------------------------------------------------------------------------

// Verifies Get decrypts and parses a SecureString parameter into credentials.
func TestRepository_Get(t *testing.T) {
	mock := &mockSSM{
		getParameterFn: func(_ context.Context, input *awsssm.GetParameterInput) (*awsssm.GetParameterOutput, error) {
			assert.Equal(t, "/prod/db/creds", *input.Name)
			assert.True(t, *input.WithDecryption)
			return &awsssm.GetParameterOutput{
				Parameter: &ssmtypes.Parameter{
					Name:    input.Name,
					Value:   aws.String(`{"username":"admin","password":"s3cret"}`),
					Version: 1,
				},
			}, nil
		},
	}

	r := New(WithClient(mock))
	creds, err := r.Get(context.Background(), "pms://prod/db/creds")
	require.NoError(t, err)
	require.NotNil(t, creds.Password())
	assert.Equal(t, "admin", creds.Password().Username())
	assert.Equal(t, "s3cret", creds.Password().Password().Reveal())
}

// Verifies Get maps ParameterNotFound to ErrNotFound.
func TestRepository_Get_NotFound(t *testing.T) {
	mock := &mockSSM{
		getParameterFn: func(_ context.Context, _ *awsssm.GetParameterInput) (*awsssm.GetParameterOutput, error) {
			return nil, &ssmtypes.ParameterNotFound{}
		},
	}

	r := New(WithClient(mock))
	_, err := r.Get(context.Background(), "pms://prod/missing")
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrNotFound))
}

// Verifies Get treats a nil parameter value as ErrNotFound.
func TestRepository_Get_NilValue(t *testing.T) {
	mock := &mockSSM{
		getParameterFn: func(_ context.Context, _ *awsssm.GetParameterInput) (*awsssm.GetParameterOutput, error) {
			return &awsssm.GetParameterOutput{
				Parameter: &ssmtypes.Parameter{Name: aws.String("/x"), Value: nil},
			}, nil
		},
	}

	r := New(WithClient(mock))
	_, err := r.Get(context.Background(), "pms://x")
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrNotFound))
}

// Verifies Create writes a SecureString parameter without overwrite.
func TestRepository_Create(t *testing.T) {
	var capturedInput *awsssm.PutParameterInput
	mock := &mockSSM{
		putParameterFn: func(_ context.Context, input *awsssm.PutParameterInput) (*awsssm.PutParameterOutput, error) {
			capturedInput = input
			return &awsssm.PutParameterOutput{Version: 1}, nil
		},
	}

	r := New(WithClient(mock))
	creds := connectivity.NewCredentialSet(pwCred("u", "p"), nil)
	err := r.Create(context.Background(), "pms://ns/path", creds)
	require.NoError(t, err)
	assert.Equal(t, "/ns/path", *capturedInput.Name)
	assert.Equal(t, ssmtypes.ParameterTypeSecureString, capturedInput.Type)
	assert.False(t, *capturedInput.Overwrite)
}

// Verifies Create maps ParameterAlreadyExists to ErrAlreadyExists.
func TestRepository_Create_AlreadyExists(t *testing.T) {
	mock := &mockSSM{
		putParameterFn: func(_ context.Context, _ *awsssm.PutParameterInput) (*awsssm.PutParameterOutput, error) {
			return nil, &ssmtypes.ParameterAlreadyExists{}
		},
	}

	r := New(WithClient(mock))
	err := r.Create(context.Background(), "pms://ns/path", connectivity.NewCredentialSet(pwCred("u", "p"), nil))
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrAlreadyExists))
}

// Verifies Update overwrites the parameter with serialized credentials.
func TestRepository_Update(t *testing.T) {
	var capturedInput *awsssm.PutParameterInput
	mock := &mockSSM{
		putParameterFn: func(_ context.Context, input *awsssm.PutParameterInput) (*awsssm.PutParameterOutput, error) {
			capturedInput = input
			return &awsssm.PutParameterOutput{Version: 2}, nil
		},
	}

	r := New(WithClient(mock))
	creds := connectivity.NewCredentialSet(pwCred("u2", "p2"), nil)
	err := r.Update(context.Background(), "pms://ns/path", creds, 0)
	require.NoError(t, err)
	assert.True(t, *capturedInput.Overwrite)
	assert.Equal(t, ssmtypes.ParameterTypeSecureString, capturedInput.Type)
}

// Verifies Update rejects a stale expected version with ErrVersionMismatch.
func TestRepository_Update_VersionMismatch(t *testing.T) {
	mock := &mockSSM{
		getParameterFn: func(_ context.Context, _ *awsssm.GetParameterInput) (*awsssm.GetParameterOutput, error) {
			return &awsssm.GetParameterOutput{
				Parameter: &ssmtypes.Parameter{Version: 5},
			}, nil
		},
	}

	r := New(WithClient(mock))
	err := r.Update(context.Background(), "pms://ns/path", connectivity.NewCredentialSet(pwCred("u", "p"), nil), 3)
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrVersionMismatch))
}

// Verifies a version-checked Update against a DELETED parameter (SDK
// returns a response with no Parameter struct) surfaces ErrNotFound —
// not ErrVersionMismatch, which would mislead the caller into a
// reload-and-retry loop against a parameter that no longer exists.
func TestRepository_Update_DeletedParameterIsNotFound(t *testing.T) {
	mock := &mockSSM{
		getParameterFn: func(_ context.Context, _ *awsssm.GetParameterInput) (*awsssm.GetParameterOutput, error) {
			return &awsssm.GetParameterOutput{Parameter: nil}, nil
		},
	}

	r := New(WithClient(mock))
	err := r.Update(context.Background(), "pms://ns/path", connectivity.NewCredentialSet(pwCred("u", "p"), nil), 3)
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrNotFound), "got %v, want ErrNotFound", err)
	assert.False(t, errors.Is(err, shared.ErrVersionMismatch), "deleted parameter must not read as version mismatch")
}

// Verifies Delete removes the parameter at the resolved path.
func TestRepository_Delete(t *testing.T) {
	var deleteCalled bool
	mock := &mockSSM{
		deleteParameterFn: func(_ context.Context, input *awsssm.DeleteParameterInput) (*awsssm.DeleteParameterOutput, error) {
			deleteCalled = true
			assert.Equal(t, "/ns/path", *input.Name)
			return &awsssm.DeleteParameterOutput{}, nil
		},
	}

	r := New(WithClient(mock))
	err := r.Delete(context.Background(), "pms://ns/path", 0)
	require.NoError(t, err)
	assert.True(t, deleteCalled)
}

// Verifies Delete with version checking succeeds only when the version matches.
func TestRepository_Delete_VersionCheck(t *testing.T) {
	mock := &mockSSM{
		getParameterFn: func(_ context.Context, _ *awsssm.GetParameterInput) (*awsssm.GetParameterOutput, error) {
			return &awsssm.GetParameterOutput{
				Parameter: &ssmtypes.Parameter{Version: 2},
			}, nil
		},
		deleteParameterFn: func(_ context.Context, _ *awsssm.DeleteParameterInput) (*awsssm.DeleteParameterOutput, error) {
			return &awsssm.DeleteParameterOutput{}, nil
		},
	}

	r := New(WithClient(mock))
	err := r.Delete(context.Background(), "pms://ns/path", 2)
	require.NoError(t, err)

	err = r.Delete(context.Background(), "pms://ns/path", 99)
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrVersionMismatch))
}

// Verifies List returns pms:// URIs for parameters under the namespace.
func TestRepository_List(t *testing.T) {
	mock := &mockSSM{
		getParametersByPathFn: func(_ context.Context, input *awsssm.GetParametersByPathInput) (*awsssm.GetParametersByPathOutput, error) {
			assert.Equal(t, "/myns", *input.Path)
			assert.True(t, *input.Recursive)
			return &awsssm.GetParametersByPathOutput{
				Parameters: []ssmtypes.Parameter{
					{Name: aws.String("/myns/db/creds")},
					{Name: aws.String("/myns/api/key")},
				},
			}, nil
		},
	}

	r := New(WithClient(mock), WithNamespace("myns"))
	uris, err := r.List(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, []string{"pms://myns/db/creds", "pms://myns/api/key"}, uris)
}

// Verifies List restricts the path prefix when a relative prefix is supplied.
func TestRepository_List_WithPrefix(t *testing.T) {
	mock := &mockSSM{
		getParametersByPathFn: func(_ context.Context, input *awsssm.GetParametersByPathInput) (*awsssm.GetParametersByPathOutput, error) {
			assert.Equal(t, "/myns/db", *input.Path)
			return &awsssm.GetParametersByPathOutput{
				Parameters: []ssmtypes.Parameter{
					{Name: aws.String("/myns/db/creds")},
				},
			}, nil
		},
	}

	r := New(WithClient(mock), WithNamespace("myns"))
	uris, err := r.List(context.Background(), "db")
	require.NoError(t, err)
	assert.Equal(t, []string{"pms://myns/db/creds"}, uris)
}

// Verifies List follows NextToken until all parameters are collected.
func TestRepository_List_Pagination(t *testing.T) {
	callCount := 0
	mock := &mockSSM{
		getParametersByPathFn: func(_ context.Context, input *awsssm.GetParametersByPathInput) (*awsssm.GetParametersByPathOutput, error) {
			callCount++
			if callCount == 1 {
				return &awsssm.GetParametersByPathOutput{
					Parameters: []ssmtypes.Parameter{{Name: aws.String("/ns/a")}},
					NextToken:  aws.String("token1"),
				}, nil
			}
			assert.Equal(t, "token1", *input.NextToken)
			return &awsssm.GetParametersByPathOutput{
				Parameters: []ssmtypes.Parameter{{Name: aws.String("/ns/b")}},
			}, nil
		},
	}

	r := New(WithClient(mock), WithNamespace("ns"))
	uris, err := r.List(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, []string{"pms://ns/a", "pms://ns/b"}, uris)
	assert.Equal(t, 2, callCount)
}

// ---------------------------------------------------------------------------
// Error mapping
// ---------------------------------------------------------------------------

// Verifies mapAWSError returns nil for a nil input.
func TestMapAWSError_Nil(t *testing.T) {
	assert.Nil(t, mapAWSError(nil))
}

// Verifies ParameterNotFound maps to ErrNotFound.
func TestMapAWSError_ParameterNotFound(t *testing.T) {
	err := mapAWSError(&ssmtypes.ParameterNotFound{})
	assert.True(t, errors.Is(err, shared.ErrNotFound))
}

// Verifies ParameterAlreadyExists maps to ErrAlreadyExists.
func TestMapAWSError_ParameterAlreadyExists(t *testing.T) {
	err := mapAWSError(&ssmtypes.ParameterAlreadyExists{})
	assert.True(t, errors.Is(err, shared.ErrAlreadyExists))
}

// Verifies unknown AWS errors map to ErrUnavailable.
func TestMapAWSError_GenericError(t *testing.T) {
	err := mapAWSError(fmt.Errorf("some AWS error"))
	assert.True(t, errors.Is(err, shared.ErrUnavailable))
}

// ---------------------------------------------------------------------------
// URI error in CRUD operations
// ---------------------------------------------------------------------------

// Verifies Get rejects non-pms URIs.
func TestRepository_Get_InvalidURI(t *testing.T) {
	r := New(WithClient(&mockSSM{}))
	_, err := r.Get(context.Background(), "vault://wrong/scheme")
	assert.Error(t, err)
}

// Verifies Create rejects invalid URIs before calling SSM.
func TestRepository_Create_InvalidURI(t *testing.T) {
	r := New(WithClient(&mockSSM{}))
	err := r.Create(context.Background(), "bad://uri", connectivity.NewCredentialSet(pwCred("u", "p"), nil))
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// HIGH-1: write round-trip guard — never persist an unreadable credential
// ---------------------------------------------------------------------------

// TestSerialize_RejectsUnreadable proves the single Create/Update write path
// (serializeCredentialSet) refuses to persist any credential set the package's
// own reader could not parse back. Without the round-trip guard each case below
// serializes to a value that every later Get / rotation poll then rejects with
// "invalid payload", turning one bad admin write into a persistent credential
// outage (HIGH-1).
//
// Mutation reasoning: deleting the ensureReadable call in serializeCredentialSet
// makes every case serialize without error, so the require.Error assertions fail.
func TestSerialize_RejectsUnreadable(t *testing.T) {
	cases := []struct {
		name  string
		creds *connectivity.CredentialSet
	}{
		{"empty set (no password, no TLS)", connectivity.NewCredentialSet(nil, nil)},
		{"empty username and empty password", connectivity.NewCredentialSet(pwCred("", ""), nil)},
		{"whitespace-only opaque secret", connectivity.NewCredentialSet(pwCred("", "  \n\t"), nil)},
		{"torn TLS: cert without key", connectivity.NewCredentialSet(nil, tlsMat("only-cert", "", nil, false))},
		{"torn TLS: key without cert", connectivity.NewCredentialSet(nil, tlsMat("", "only-key", nil, false))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := serializeCredentialSet(tc.creds)
			require.Error(t, err)
			assert.True(t, errors.Is(err, shared.ErrInvalidPayload), "want ErrInvalidPayload, got %v", err)
		})
	}
}

// TestRepository_Create_RejectsUnreadableBeforePut proves an admin Create of an
// unusable credential set is rejected BEFORE any PutParameter, so a bad write
// can never reach SSM and become a value every later Get fails to parse (HIGH-1).
func TestRepository_Create_RejectsUnreadableBeforePut(t *testing.T) {
	putCalled := false
	mock := &mockSSM{
		putParameterFn: func(_ context.Context, _ *awsssm.PutParameterInput) (*awsssm.PutParameterOutput, error) {
			putCalled = true
			return &awsssm.PutParameterOutput{}, nil
		},
	}
	r := New(WithClient(mock))
	err := r.Create(context.Background(), "pms://ns/path", connectivity.NewCredentialSet(nil, nil))
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrInvalidPayload), "want ErrInvalidPayload, got %v", err)
	assert.False(t, putCalled, "PutParameter must not run for an unreadable credential set")
}

// TestRepository_Update_RejectsUnreadableBeforePut is the Update counterpart of
// the HIGH-1 guard: a torn-TLS set must not overwrite a live parameter with a
// value the reader would reject on the next rotation poll.
func TestRepository_Update_RejectsUnreadableBeforePut(t *testing.T) {
	putCalled := false
	mock := &mockSSM{
		putParameterFn: func(_ context.Context, _ *awsssm.PutParameterInput) (*awsssm.PutParameterOutput, error) {
			putCalled = true
			return &awsssm.PutParameterOutput{}, nil
		},
	}
	r := New(WithClient(mock))
	err := r.Update(context.Background(), "pms://ns/path",
		connectivity.NewCredentialSet(nil, tlsMat("cert-only", "", nil, false)), 0)
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrInvalidPayload), "want ErrInvalidPayload, got %v", err)
	assert.False(t, putCalled, "PutParameter must not run for a torn-TLS credential set")
}

// ---------------------------------------------------------------------------
// HIGH-2: opaque password-only credentials (e.g. Service Bus SAS strings)
// ---------------------------------------------------------------------------

// sasConnString is a representative Azure Service Bus SAS connection string —
// an opaque single-value secret with no username.
const sasConnString = "Endpoint=sb://ns.servicebus.windows.net/;SharedAccessKeyName=root;SharedAccessKey=abc123=="

// TestParseCredentials_OpaqueSecret proves the explicit password-only "secret"
// JSON shape parses into a PasswordCredential with an EMPTY username and the
// whole value in the password — the runtime shape the Service Bus transport
// consumes for a SAS connection string (HIGH-2).
//
// Mutation reasoning: before the fix, declaredCapabilities knew only
// password/tls, so a "secret"-typed document matched no capability and
// parseJSONCredentials returned "unable to determine credential type" — every
// require.NoError below fails.
func TestParseCredentials_OpaqueSecret(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"secret token + secret field", `{"type":"secret","secret":"` + sasConnString + `"}`},
		{"secret token + password field", `{"type":"secret","password":"` + sasConnString + `"}`},
		{"opaque alias", `{"type":"opaque","secret":"` + sasConnString + `"}`},
		{"sas alias", `{"type":"sas","secret":"` + sasConnString + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			creds, err := parseCredentials(tc.input)
			require.NoError(t, err)
			require.NotNil(t, creds.Password())
			assert.Equal(t, "", creds.Password().Username(), "opaque secret must have an empty username")
			assert.Equal(t, sasConnString, creds.Password().Password().Reveal())
			assert.Nil(t, creds.TLS())
		})
	}
}

// TestParseCredentials_SecretTokenWithUsername_BackwardCompatible proves a
// secret/opaque/sas type token that appears ALONGSIDE a username parses as an
// ordinary username/password credential, NOT as an opaque secret. Readers that
// predate the opaque shape ignored these tokens and parsed such documents as
// username/password; stored SSM values are not rewritten on upgrade, so this
// read must stay backward compatible (the invariant: any JSON an earlier reader
// accepted still parses to the same credential).
//
// Mutation reasoning: if the opaque shape were selected on the type token alone
// (declaredSecret) instead of "declaredSecret AND no username", each row below
// would be rejected as a username-bearing opaque secret and every
// require.NoError fails — the exact upgrade regression this guards against.
func TestParseCredentials_SecretTokenWithUsername_BackwardCompatible(t *testing.T) {
	cases := []struct {
		name, input, wantUser, wantPass string
	}{
		{"sas token + username/password", `{"type":"sas","username":"broker","password":"p"}`, "broker", "p"},
		{"opaque token + user/pass aliases", `{"type":"opaque","user":"u","pass":"q"}`, "u", "q"},
		{"secret token + username/password", `{"type":"secret","username":"root","password":"conn"}`, "root", "conn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			creds, err := parseCredentials(tc.input)
			require.NoError(t, err)
			require.NotNil(t, creds.Password())
			assert.Equal(t, tc.wantUser, creds.Password().Username())
			assert.Equal(t, tc.wantPass, creds.Password().Password().Reveal())
			assert.Nil(t, creds.TLS())
		})
	}
}

// TestParseCredentials_OpaqueSecret_RejectsEmptyOrBlank proves a "secret" shape
// whose value is missing, empty, or WHITESPACE-ONLY is rejected rather than
// yielding an anonymous no-auth credential or a semantically empty connection
// string that fails broker client creation downstream.
//
// Mutation reasoning: relaxing the opaque-value guard from
// strings.TrimSpace(password) == "" back to password == "" lets the two
// whitespace rows below parse successfully, so their require.Error fails.
func TestParseCredentials_OpaqueSecret_RejectsEmptyOrBlank(t *testing.T) {
	for _, input := range []string{
		`{"type":"secret"}`,
		`{"type":"secret","secret":""}`,
		`{"type":"secret","secret":"   "}`,
		`{"type":"sas","secret":"\n\t"}`,
	} {
		t.Run(input, func(t *testing.T) {
			_, err := parseCredentials(input)
			require.Error(t, err)
		})
	}
}

// TestSerializeAndParseRoundTrip_OpaqueSecret proves a password-only credential
// (empty username) survives serialize→parse intact, so SSM can both store and
// rotate a Service Bus SAS connection string in the documented runtime shape
// (HIGH-2). It also confirms the HIGH-1 write guard accepts this legitimate
// shape rather than rejecting it as username-less.
func TestSerializeAndParseRoundTrip_OpaqueSecret(t *testing.T) {
	original := connectivity.NewCredentialSet(pwCred("", sasConnString), nil)

	s, err := serializeCredentialSet(original)
	require.NoError(t, err)

	parsed, err := parseCredentials(s)
	require.NoError(t, err)
	require.NotNil(t, parsed.Password())
	assert.Equal(t, "", parsed.Password().Username())
	assert.Equal(t, sasConnString, parsed.Password().Password().Reveal())
	assert.True(t, parsed.Equal(original), "opaque secret must round-trip to an equal credential set")
}

// TestRepository_OpaqueSecret_CreateGetRoundTrip proves the end-to-end HIGH-2
// guarantee against an in-memory SSM mock: an admin Create of a password-only
// (opaque SAS) credential persists a value that Get reads back intact.
func TestRepository_OpaqueSecret_CreateGetRoundTrip(t *testing.T) {
	var stored string
	mock := &mockSSM{
		putParameterFn: func(_ context.Context, input *awsssm.PutParameterInput) (*awsssm.PutParameterOutput, error) {
			stored = *input.Value
			return &awsssm.PutParameterOutput{Version: 1}, nil
		},
		getParameterFn: func(_ context.Context, _ *awsssm.GetParameterInput) (*awsssm.GetParameterOutput, error) {
			return &awsssm.GetParameterOutput{
				Parameter: &ssmtypes.Parameter{Value: aws.String(stored), Version: 1},
			}, nil
		},
	}
	r := New(WithClient(mock))
	require.NoError(t, r.Create(context.Background(), "pms://ns/sb",
		connectivity.NewCredentialSet(pwCred("", sasConnString), nil)))

	got, err := r.Get(context.Background(), "pms://ns/sb")
	require.NoError(t, err)
	require.NotNil(t, got.Password())
	assert.Equal(t, "", got.Password().Username())
	assert.Equal(t, sasConnString, got.Password().Password().Reveal())
}

// ---------------------------------------------------------------------------
// HIGH-3 (source-side support): Get is always uncached
// ---------------------------------------------------------------------------

// TestRepository_Get_NoInternalCache proves each Get fetches fresh from SSM
// rather than serving a cached value. This is the source-side property the
// reactive re-resolve path (HIGH-3) relies on: when a live transport reports a
// broker auth failure and the refresher forces an out-of-band re-resolve
// (runtime/credentials PollBasedWrapper.Refresh -> ResolveUncached ->
// repository.Get), the repository must observe the rotated parameter, never a
// stale copy. The transport->refresher wiring itself is a cross-cutting
// dependency outside this module; this test guards the leaf the wiring depends on.
func TestRepository_Get_NoInternalCache(t *testing.T) {
	value := `{"username":"u","password":"old"}`
	mock := &mockSSM{
		getParameterFn: func(_ context.Context, _ *awsssm.GetParameterInput) (*awsssm.GetParameterOutput, error) {
			return &awsssm.GetParameterOutput{
				Parameter: &ssmtypes.Parameter{Value: aws.String(value), Version: 1},
			}, nil
		},
	}
	r := New(WithClient(mock))

	first, err := r.Get(context.Background(), "pms://ns/p")
	require.NoError(t, err)
	require.NotNil(t, first.Password())
	assert.Equal(t, "old", first.Password().Password().Reveal())

	// Simulate an out-of-band rotation of the backing parameter.
	value = `{"username":"u","password":"new"}`

	second, err := r.Get(context.Background(), "pms://ns/p")
	require.NoError(t, err)
	require.NotNil(t, second.Password())
	assert.Equal(t, "new", second.Password().Password().Reveal(),
		"Get must re-read the backend, not serve a cached value")
}

// pwCred builds a *connectivity.PasswordCredential from the immutable value
// constructor for use in test credential sets.
func pwCred(username, password string) *connectivity.PasswordCredential {
	c := connectivity.NewPasswordCredential(username, password)
	return &c
}

// tlsMat builds a *connectivity.TLSMaterial from the immutable value
// constructor for use in test credential sets.
func tlsMat(certPEM, keyPEM string, caPEMs []string, insecure bool) *connectivity.TLSMaterial {
	m := connectivity.NewTLSMaterial(certPEM, keyPEM, caPEMs, insecure)
	return &m
}
