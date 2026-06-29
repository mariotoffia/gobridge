package ssm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/localstack"
)

func TestMain(m *testing.M) {
	localstack.Configure(
		localstack.WithServices("ssm"),
		localstack.WithCleanOrphans(true),
	)
	code := m.Run()
	localstack.Shutdown()
	os.Exit(code)
}

func uniqueURI(prefix string) string {
	return fmt.Sprintf("pms://test/%s/%d", prefix, time.Now().UnixNano())
}

// Verifies full Create → Get round-trip for password credentials against LocalStack SSM.
func TestIntegration_SSM_CreateAndGet_Password(t *testing.T) {
	ep := localstack.Endpoint(t)
	repo := New(WithEndpoint(ep), WithRegion("us-west-1"))
	ctx := context.Background()
	uri := uniqueURI("password")

	creds := connectivity.NewCredentialSet(pwCred("admin", "s3cret!"), nil)

	require.NoError(t, repo.Create(ctx, uri, creds))

	got, err := repo.Get(ctx, uri)
	require.NoError(t, err)
	require.NotNil(t, got.Password())
	assert.Equal(t, "admin", got.Password().Username())
	assert.Equal(t, "s3cret!", got.Password().Password().Reveal())
}

// Verifies full Create → Get round-trip for TLS credentials against LocalStack SSM.
func TestIntegration_SSM_CreateAndGet_TLS(t *testing.T) {
	ep := localstack.Endpoint(t)
	repo := New(WithEndpoint(ep), WithRegion("us-west-1"))
	ctx := context.Background()
	uri := uniqueURI("tls")

	creds := connectivity.NewCredentialSet(nil, tlsMat(
		"-----BEGIN CERTIFICATE-----\ntest-cert\n-----END CERTIFICATE-----",
		"-----BEGIN PRIVATE KEY-----\ntest-key\n-----END PRIVATE KEY-----",
		[]string{"-----BEGIN CERTIFICATE-----\nca1\n-----END CERTIFICATE-----"},
		false,
	))

	require.NoError(t, repo.Create(ctx, uri, creds))

	got, err := repo.Get(ctx, uri)
	require.NoError(t, err)
	require.NotNil(t, got.TLS())
	assert.Equal(t, creds.TLS().CertPEM(), got.TLS().CertPEM())
	assert.Equal(t, creds.TLS().KeyPEM().Reveal(), got.TLS().KeyPEM().Reveal())
	assert.Equal(t, creds.TLS().CAPEMs(), got.TLS().CAPEMs())
	assert.Equal(t, creds.TLS().InsecureSkipVerify(), got.TLS().InsecureSkipVerify())
}

// Verifies Create on an existing parameter returns ErrAlreadyExists.
func TestIntegration_SSM_Create_AlreadyExists(t *testing.T) {
	ep := localstack.Endpoint(t)
	repo := New(WithEndpoint(ep), WithRegion("us-west-1"))
	ctx := context.Background()
	uri := uniqueURI("dup")

	creds := connectivity.NewCredentialSet(pwCred("u", "p"), nil)

	require.NoError(t, repo.Create(ctx, uri, creds))
	err := repo.Create(ctx, uri, creds)
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrAlreadyExists), "expected ErrAlreadyExists, got: %v", err)
}

// Verifies Update overwrites the parameter and Get returns the new value.
func TestIntegration_SSM_Update(t *testing.T) {
	ep := localstack.Endpoint(t)
	repo := New(WithEndpoint(ep), WithRegion("us-west-1"))
	ctx := context.Background()
	uri := uniqueURI("update")

	original := connectivity.NewCredentialSet(pwCred("u1", "p1"), nil)
	require.NoError(t, repo.Create(ctx, uri, original))

	updated := connectivity.NewCredentialSet(pwCred("u2", "p2"), nil)
	require.NoError(t, repo.Update(ctx, uri, updated, 0))

	got, err := repo.Get(ctx, uri)
	require.NoError(t, err)
	require.NotNil(t, got.Password())
	assert.Equal(t, "u2", got.Password().Username())
	assert.Equal(t, "p2", got.Password().Password().Reveal())
}

// Verifies Delete removes the parameter so Get returns ErrNotFound.
func TestIntegration_SSM_Delete(t *testing.T) {
	ep := localstack.Endpoint(t)
	repo := New(WithEndpoint(ep), WithRegion("us-west-1"))
	ctx := context.Background()
	uri := uniqueURI("delete")

	creds := connectivity.NewCredentialSet(pwCred("u", "p"), nil)
	require.NoError(t, repo.Create(ctx, uri, creds))

	require.NoError(t, repo.Delete(ctx, uri, 0))

	_, err := repo.Get(ctx, uri)
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrNotFound), "expected ErrNotFound, got: %v", err)
}

// Verifies Get on a non-existent parameter returns ErrNotFound.
func TestIntegration_SSM_Get_NotFound(t *testing.T) {
	ep := localstack.Endpoint(t)
	repo := New(WithEndpoint(ep), WithRegion("us-west-1"))
	ctx := context.Background()

	_, err := repo.Get(ctx, uniqueURI("nonexistent"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrNotFound), "expected ErrNotFound, got: %v", err)
}

// Verifies List returns URIs for parameters created under the namespace.
func TestIntegration_SSM_List(t *testing.T) {
	ep := localstack.Endpoint(t)
	ns := fmt.Sprintf("listns-%d", time.Now().UnixNano())
	repo := New(WithEndpoint(ep), WithRegion("us-west-1"), WithNamespace(ns))
	ctx := context.Background()

	creds := connectivity.NewCredentialSet(pwCred("u", "p"), nil)

	uri1 := fmt.Sprintf("pms://%s/db/primary", ns)
	uri2 := fmt.Sprintf("pms://%s/db/replica", ns)
	uri3 := fmt.Sprintf("pms://%s/api/key", ns)

	require.NoError(t, repo.Create(ctx, uri1, creds))
	require.NoError(t, repo.Create(ctx, uri2, creds))
	require.NoError(t, repo.Create(ctx, uri3, creds))

	uris, err := repo.List(ctx, "")
	require.NoError(t, err)
	assert.Len(t, uris, 3, "expected 3 URIs, got %d: %v", len(uris), uris)

	urisDB, err := repo.List(ctx, "db")
	require.NoError(t, err)
	assert.Len(t, urisDB, 2, "expected 2 URIs under db/, got %d: %v", len(urisDB), urisDB)
}

// Verifies Update with version mismatch returns ErrVersionMismatch.
func TestIntegration_SSM_Update_VersionMismatch(t *testing.T) {
	ep := localstack.Endpoint(t)
	repo := New(WithEndpoint(ep), WithRegion("us-west-1"))
	ctx := context.Background()
	uri := uniqueURI("vcheck")

	creds := connectivity.NewCredentialSet(pwCred("u", "p"), nil)
	require.NoError(t, repo.Create(ctx, uri, creds))

	updated := connectivity.NewCredentialSet(pwCred("u2", "p2"), nil)
	err := repo.Update(ctx, uri, updated, 999)
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrVersionMismatch), "expected ErrVersionMismatch, got: %v", err)
}
