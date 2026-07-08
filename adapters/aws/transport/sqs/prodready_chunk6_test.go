package sqs

import (
	"context"
	"errors"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// recordingExporter is a minimal ports.MetricsExporter used to prove the
// factory threads a real exporter into the Receiver/Sender (Finding 8).
type recordingExporter struct{ ports.NoopExporter }

// Finding 8 (HIGH) — factory dead-metrics wiring.
//
// NewFactory previously accepted no exporter and never set cfg.Metrics,
// so every Receiver/Sender it built fell back to a Noop exporter and all
// nine SQS metrics were dead on the config-driven/plugin path. NewFactory
// now mirrors paho.NewFactory: the variadic exporter is threaded into
// every ReceiverConfig/SenderConfig. These tests fail-without (metrics
// stays the injected Noop the adapter substitutes) / pass-with (the
// receiver/sender holds the exact injected exporter).
func TestFactory_ThreadsMetricsIntoReceiver(t *testing.T) {
	exp := &recordingExporter{}
	f := NewFactory(nil, exp)

	recv, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "r",
		Config: Config{QueueURL: "https://q"},
	}, nil)
	require.NoError(t, err)

	r, ok := recv.(*Receiver)
	require.True(t, ok)
	assert.True(t, ports.MetricsExporter(exp) == r.metrics,
		"factory must thread the exporter into the receiver so its metrics emit")
}

func TestFactory_ThreadsMetricsIntoSender(t *testing.T) {
	exp := &recordingExporter{}
	f := NewFactory(nil, exp)

	snd, err := f.NewSender(context.Background(), ports.SenderSpec{
		ID:     "s",
		Config: Config{QueueURL: "https://q"},
	}, nil)
	require.NoError(t, err)

	s, ok := snd.(*Sender)
	require.True(t, ok)
	assert.True(t, ports.MetricsExporter(exp) == s.metrics,
		"factory must thread the exporter into the sender so its metrics emit")
}

// TestFactory_NoMetricsStillNoop proves the variadic keeps single-arg
// callers compiling and falls back to the Noop exporter (never nil).
func TestFactory_NoMetricsStillNoop(t *testing.T) {
	f := NewFactory(nil)
	recv, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "r",
		Config: Config{QueueURL: "https://q"},
	}, nil)
	require.NoError(t, err)
	r := recv.(*Receiver)
	require.NotNil(t, r.metrics)
	_, isNoop := r.metrics.(*ports.NoopExporter)
	assert.True(t, isNoop, "no exporter → Noop, never nil")
}

// Finding 3 (HIGH) — credentials_uri material silently discarded.
//
// ApplyCredentials used to clear CredentialsURIRef and drop the resolved
// material, so the INITIAL client always fell back to the ambient SDK
// chain. It now retains long-term static material and projects it into
// ReceiverConfig/SenderConfig.InitialCredentials so ensureClient builds
// the first client with it. Fail-without: InitialCredentials is nil.
func TestApplyCredentials_ThreadsInitialCredentials(t *testing.T) {
	c := &Config{QueueURL: "https://q", CredentialsURIRef: "secret://sqs"}
	pw := connectivity.NewPasswordCredential("AKIAEXAMPLE", "long-term-secret")
	set := connectivity.NewCredentialSet(&pw, nil)

	require.NoError(t, c.ApplyCredentials(set))
	assert.Empty(t, c.CredentialsURIRef, "resolved URI must be cleared after apply")

	rc := c.toReceiverConfig()
	require.NotNil(t, rc.InitialCredentials,
		"receiver initial client must be built from the resolved credentials_uri material")
	assert.Equal(t, "AKIAEXAMPLE", rc.InitialCredentials.Username())

	sc := c.toSenderConfig()
	require.NotNil(t, sc.InitialCredentials,
		"sender initial client must be built from the resolved credentials_uri material")
	assert.Equal(t, "AKIAEXAMPLE", sc.InitialCredentials.Username())
}

// Finding 6 (MEDIUM) — temporary/STS credentials brick the client.
//
// A connectivity.CredentialSet carries only username/password, no session
// token. Applying an ASIA-prefixed (STS) access key via a static provider
// with an empty token yields a client that fails every request. Reject the
// material up front instead of silently degrading a working client.
func TestApplyCredentials_RejectsTemporarySTSMaterial(t *testing.T) {
	c := &Config{QueueURL: "https://q", CredentialsURIRef: "secret://sqs"}
	pw := connectivity.NewPasswordCredential("ASIATEMPORARY", "session-scoped-secret")
	set := connectivity.NewCredentialSet(&pw, nil)

	err := c.ApplyCredentials(set)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTemporaryCredentialsUnsupported)
	assert.Nil(t, c.toReceiverConfig().InitialCredentials,
		"temporary material must not be retained as an initial credential")
}

// rebuildSQSClient rejects temporary material and surfaces it as
// ErrNotAuthorized so rotation callers classify it consistently and leave
// the existing client in place.
func TestRebuildSQSClient_RejectsTemporarySTSMaterial(t *testing.T) {
	pw := connectivity.NewPasswordCredential("ASIATEMPORARY", "session-scoped-secret")
	client, err := rebuildSQSClient(context.Background(), "eu-west-1", "", "", &pw)
	require.Error(t, err)
	assert.Nil(t, client)
	assert.ErrorIs(t, err, ErrTemporaryCredentialsUnsupported)
	assert.ErrorIs(t, err, shared.ErrNotAuthorized)
}

// Sender.ApplyCredentials must leave the previously-working client in
// place when handed temporary material — never swap in a bricked client.
func TestSender_ApplyCredentials_TemporaryPreservesClient(t *testing.T) {
	mock := &mockSQSClient{}
	snd, err := NewSender(SenderConfig{QueueURL: "https://q", Client: mock})
	require.NoError(t, err)
	before := snd.loadClient()

	pw := connectivity.NewPasswordCredential("ASIATEMPORARY", "session-scoped-secret")
	set := connectivity.NewCredentialSet(&pw, nil)
	err = snd.ApplyCredentials(context.Background(), set)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTemporaryCredentialsUnsupported)
	assert.True(t, before == snd.loadClient(), "a rejected rotation must not swap the client")
}

// Finding 5 (MEDIUM) — delay_seconds>0 + FIFO passes build then every
// send fails. AWS rejects a non-zero DelaySeconds on a FIFO send entry.
// Reject the combination in validate() (mirrors the FIFO group check).
func TestSenderConfig_DelayPlusFIFO_Rejected(t *testing.T) {
	t.Run("fifo_suffix", func(t *testing.T) {
		c := SenderConfig{QueueURL: "https://q.fifo", MessageGroupID: "g", DelaySeconds: 5}
		require.Error(t, c.validate())
	})
	t.Run("explicit_fifo_flag", func(t *testing.T) {
		c := SenderConfig{QueueURL: "https://q", FIFO: true, DelaySeconds: 5}
		require.Error(t, c.validate())
	})
	t.Run("delay_on_standard_ok", func(t *testing.T) {
		c := SenderConfig{QueueURL: "https://q", DelaySeconds: 5}
		require.NoError(t, c.validate())
	})
	t.Run("fifo_without_delay_ok", func(t *testing.T) {
		c := SenderConfig{QueueURL: "https://q.fifo", MessageGroupID: "g"}
		require.NoError(t, c.validate())
	})
}

// NewSender must fail fast for the delay+FIFO combination.
func TestNewSender_DelayPlusFIFO_Rejected(t *testing.T) {
	_, err := NewSender(SenderConfig{QueueURL: "https://q.fifo", MessageGroupID: "g", DelaySeconds: 5})
	require.Error(t, err)
}

// Finding 12 (MINOR) — wait_time_seconds:0 / max_messages:0 silently
// coerced. The registry decoder decodes into a DefaultConfig() so an
// OMITTED key keeps the documented default while an EXPLICIT 0 is rejected
// with a clear error instead of being silently coerced back.
func TestRegistryDecoder_RejectsExplicitZeros(t *testing.T) {
	reg := ports.NewRegistry()
	require.NoError(t, Register(reg))

	t.Run("explicit_wait_time_zero_rejected", func(t *testing.T) {
		_, err := reg.Decode("sqs", rawMap{"queue_url": "https://q", "wait_time_seconds": 0})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "wait_time_seconds")
	})
	t.Run("explicit_max_messages_zero_rejected", func(t *testing.T) {
		_, err := reg.Decode("sqs", rawMap{"queue_url": "https://q", "max_messages": 0})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max_messages")
	})
	t.Run("omitted_uses_defaults", func(t *testing.T) {
		pc, err := reg.Decode("sqs", rawMap{"queue_url": "https://q"})
		require.NoError(t, err)
		c := pc.(*Config)
		assert.Equal(t, int32(20), c.WaitTimeSeconds)
		assert.Equal(t, int32(10), c.MaxMessages)
	})
	t.Run("explicit_valid_values_survive", func(t *testing.T) {
		pc, err := reg.Decode("sqs", rawMap{"queue_url": "https://q", "wait_time_seconds": 5, "max_messages": 3})
		require.NoError(t, err)
		c := pc.(*Config)
		assert.Equal(t, int32(5), c.WaitTimeSeconds)
		assert.Equal(t, int32(3), c.MaxMessages)
	})
}

// rawMap is a trivial ports.RawConfig backed by a decoded map so the
// registry decoder can be exercised without a YAML source.
type rawMap map[string]any

func (m rawMap) Decode(target any) error {
	c, ok := target.(*Config)
	if !ok {
		return errors.New("rawMap: unexpected target")
	}
	// Emulate mapstructure: only keys PRESENT in the input are assigned,
	// so omitted keys retain the DefaultConfig() pre-fill.
	if v, ok := m["queue_url"]; ok {
		c.QueueURL = v.(string)
	}
	if v, ok := m["wait_time_seconds"]; ok {
		c.WaitTimeSeconds = int32(v.(int))
	}
	if v, ok := m["max_messages"]; ok {
		c.MaxMessages = int32(v.(int))
	}
	return nil
}

// Finding 3 (MEDIUM) — auto-extend interval leaves margin after one
// failure. The tick fires at vis/3 (not vis/2) so a retry at the next
// tick (2·vis/3) still lands strictly before the window lapses at vis.
func TestAutoExtendInterval_IsOneThirdOfVisibility(t *testing.T) {
	assert.Equal(t, 10*time.Second, autoExtendInterval(30))
	assert.Equal(t, 2*time.Second, autoExtendInterval(6))
	// Floored at 1s so the clock Ticker never receives d <= 0.
	assert.Equal(t, time.Second, autoExtendInterval(3))
	assert.Equal(t, time.Second, autoExtendInterval(1))
}

// Finding 9 (MINOR) — deprecated AttributeNames on receive. The runtime
// retry cap depends on ApproximateReceiveCount, which rides the system
// attributes. Migrate to MessageSystemAttributeNames = [All].
func TestReceive_UsesMessageSystemAttributeNames(t *testing.T) {
	var captured *awssqs.ReceiveMessageInput
	mock := &mockSQSClient{
		ReceiveMessageFn: func(_ context.Context, in *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
			captured = in
			return &awssqs.ReceiveMessageOutput{}, nil
		},
	}
	noAutoExtend := false
	recv, err := NewReceiver(ReceiverConfig{
		QueueURL:          "https://q",
		VisibilityTimeout: 30,
		AutoExtend:        &noAutoExtend,
		Client:            mock,
	}, nil)
	require.NoError(t, err)
	require.NoError(t, recv.ensureClient(context.Background()))

	_, err = recv.pollAndConvert(context.Background(), "https://q", recv.cfg.MaxMessages, time.Second)
	require.NoError(t, err)
	require.NotNil(t, captured)

	assert.Contains(t, captured.MessageSystemAttributeNames,
		sqstypes.MessageSystemAttributeNameAll,
		"receive must request all system attributes (ApproximateReceiveCount)")
}
