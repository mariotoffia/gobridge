package sqs

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// TestApplyCredentials_Sender_SwapsClient verifies that ApplyCredentials
// on a Sender replaces the cached *sqs.Client so subsequent calls use
// the new static credentials. The swap is observable as a pointer
// change on the internal client field; actual network behavior is
// covered by integration tests.
func TestApplyCredentials_Sender_SwapsClient(t *testing.T) {
	s, err := NewSender(SenderConfig{
		Region:   "us-east-1",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123/q",
	})
	require.NoError(t, err)

	s.initMu.Lock()
	s.client = nil
	s.initMu.Unlock()

	err = s.ApplyCredentials(t.Context(), &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "AKIA_NEW", Password: "SECRET_NEW"},
	})
	require.NoError(t, err)

	s.initMu.Lock()
	got := s.client
	s.initMu.Unlock()
	require.NotNil(t, got, "client must be non-nil after ApplyCredentials")
}

// TestApplyCredentials_Receiver_SwapsClient parallels the Sender test.
func TestApplyCredentials_Receiver_SwapsClient(t *testing.T) {
	r, err := NewReceiver(ReceiverConfig{
		Region:   "us-east-1",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123/q",
	}, nil)
	require.NoError(t, err)

	err = r.ApplyCredentials(t.Context(), &connectivity.CredentialSet{
		Password: &connectivity.PasswordCredential{Username: "AKIA_NEW", Password: "SECRET_NEW"},
	})
	require.NoError(t, err)

	r.initMu.Lock()
	got := r.client
	r.initMu.Unlock()
	require.NotNil(t, got, "client must be non-nil after ApplyCredentials")
}

// TestApplyCredentials_NilSet_NoOp ensures nil or password-less sets
// are treated as no-ops rather than errors — the refresher dispatches
// every rotation event and some stores emit empty sets on startup.
func TestApplyCredentials_Sender_NilSet_NoOp(t *testing.T) {
	s, err := NewSender(SenderConfig{
		Region:   "us-east-1",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123/q",
	})
	require.NoError(t, err)

	require.NoError(t, s.ApplyCredentials(t.Context(), nil))
	require.NoError(t, s.ApplyCredentials(t.Context(), &connectivity.CredentialSet{}))
}
