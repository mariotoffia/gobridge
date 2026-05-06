package ssm

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			require.NotNil(t, creds.Password)
			assert.Equal(t, tt.username, creds.Password.Username)
			assert.Equal(t, tt.password, creds.Password.Password)
			assert.Nil(t, creds.TLS)
		})
	}
}

// Verifies colon-separated user:password strings parse correctly.
func TestParseCredentials_SimpleFormat(t *testing.T) {
	creds, err := parseCredentials("myuser:mypassword")
	require.NoError(t, err)
	require.NotNil(t, creds.Password)
	assert.Equal(t, "myuser", creds.Password.Username)
	assert.Equal(t, "mypassword", creds.Password.Password)
}

// Verifies simple format keeps colons in the password portion after the first separator.
func TestParseCredentials_SimpleFormat_PasswordWithColon(t *testing.T) {
	creds, err := parseCredentials("admin:pass:word:123")
	require.NoError(t, err)
	require.NotNil(t, creds.Password)
	assert.Equal(t, "admin", creds.Password.Username)
	assert.Equal(t, "pass:word:123", creds.Password.Password)
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
	require.NotNil(t, creds.TLS)
	assert.Nil(t, creds.Password)
	assert.NotEmpty(t, creds.TLS.CertPEM)
	assert.NotEmpty(t, creds.TLS.KeyPEM)
	assert.Len(t, creds.TLS.CAPEMs, 1)
	assert.False(t, creds.TLS.InsecureSkipVerify)
}

// Verifies TLS JSON sets InsecureSkipVerify when insecure is true.
func TestParseCredentials_TLS_InsecureSkipVerify(t *testing.T) {
	input := `{"certPem":"cert","keyPem":"key","insecure":true}`
	creds, err := parseCredentials(input)
	require.NoError(t, err)
	require.NotNil(t, creds.TLS)
	assert.True(t, creds.TLS.InsecureSkipVerify)
}

// Verifies a single ca string field is accepted as one CA PEM entry.
func TestParseCredentials_TLS_SingleCAPem(t *testing.T) {
	input := `{"certPem":"cert","keyPem":"key","ca":"single-ca"}`
	creds, err := parseCredentials(input)
	require.NoError(t, err)
	require.NotNil(t, creds.TLS)
	assert.Equal(t, []string{"single-ca"}, creds.TLS.CAPEMs)
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

// ---------------------------------------------------------------------------
// Serialization round-trip
// ---------------------------------------------------------------------------

// Verifies serializeCredentialSet and parseCredentials round-trip password credentials.
func TestSerializeAndParseRoundTrip_Password(t *testing.T) {
	original := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "u", Password: "p"},
	}
	s, err := serializeCredentialSet(original)
	require.NoError(t, err)

	parsed, err := parseCredentials(s)
	require.NoError(t, err)
	require.NotNil(t, parsed.Password)
	assert.Equal(t, "u", parsed.Password.Username)
	assert.Equal(t, "p", parsed.Password.Password)
}

// Verifies serializeCredentialSet and parseCredentials round-trip TLS material.
func TestSerializeAndParseRoundTrip_TLS(t *testing.T) {
	original := &domain.CredentialSet{
		TLS: &domain.TLSMaterial{
			CertPEM:            "cert-data",
			KeyPEM:             "key-data",
			CAPEMs:             []string{"ca1", "ca2"},
			InsecureSkipVerify: true,
		},
	}
	s, err := serializeCredentialSet(original)
	require.NoError(t, err)

	parsed, err := parseCredentials(s)
	require.NoError(t, err)
	require.NotNil(t, parsed.TLS)
	assert.Equal(t, "cert-data", parsed.TLS.CertPEM)
	assert.Equal(t, "key-data", parsed.TLS.KeyPEM)
	assert.Equal(t, []string{"ca1", "ca2"}, parsed.TLS.CAPEMs)
	assert.True(t, parsed.TLS.InsecureSkipVerify)
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
	require.NotNil(t, creds.Password)
	assert.Equal(t, "admin", creds.Password.Username)
	assert.Equal(t, "s3cret", creds.Password.Password)
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
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "u", Password: "p"},
	}
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
	err := r.Create(context.Background(), "pms://ns/path", &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "u", Password: "p"},
	})
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
	creds := &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "u2", Password: "p2"},
	}
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
	err := r.Update(context.Background(), "pms://ns/path", &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "u", Password: "p"},
	}, 3)
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrVersionMismatch))
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
	err := r.Create(context.Background(), "bad://uri", &domain.CredentialSet{
		Password: &domain.PasswordCredential{Username: "u", Password: "p"},
	})
	assert.Error(t, err)
}
