package paho

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// The SDK returns BOTH the acknowledgement and a generic error whenever a
// SUBACK / UNSUBACK / PUBACK carries a reason code of 0x80 or higher. The
// broker's verdict lives only in that reason code, so the adapter must
// classify the code FIRST and fall back to the SDK error only when no
// acknowledgement arrived at all. Discarding the code turns "not authorized"
// (permanent) into "unavailable" (transient), which retries a denial until
// the replay budget is spent and then reports the wrong cause.

// ackAndErrorConn is a pahoConnection double that mirrors the SDK's
// "acknowledgement AND error" return shape for rejected reason codes.
type ackAndErrorConn struct {
	// subReasonByTopic maps a requested filter to the reason code the broker
	// answers with. Keying by topic rather than by position keeps the test
	// deterministic: reconcile derives the SUBSCRIBE order from a map, so a
	// positional reason vector would attach the rejection to a different
	// filter on different runs.
	subReasonByTopic map[string]byte
	subErr           error
	unsubReasons     []byte
	unsubErr         error
	publish          publishResult
	publishErr       error
}

func (c *ackAndErrorConn) AwaitConnection(context.Context) error { return nil }
func (c *ackAndErrorConn) Disconnect(context.Context) error      { return nil }

func (c *ackAndErrorConn) Subscribe(_ context.Context, subs []subscribeSpec) ([]byte, error) {
	reasons := make([]byte, len(subs))
	for i, s := range subs {
		if reason, ok := c.subReasonByTopic[s.Topic]; ok {
			reasons[i] = reason
			continue
		}
		reasons[i] = s.QoS
	}
	if c.subReasonByTopic == nil && c.subErr == nil {
		return reasons, nil
	}
	return reasons, c.subErr
}

func (c *ackAndErrorConn) Unsubscribe(_ context.Context, topics []string) ([]byte, error) {
	if c.unsubReasons == nil && c.unsubErr == nil {
		return make([]byte, len(topics)), nil
	}
	return c.unsubReasons, c.unsubErr
}

func (c *ackAndErrorConn) PublishEnvelope(
	context.Context, *messaging.Envelope, string, SenderOptions, clock.Clock,
) (publishResult, error) {
	return c.publish, c.publishErr
}

func (c *ackAndErrorConn) Underlying() *autopaho.ConnectionManager { return nil }

var _ pahoConnection = (*ackAndErrorConn)(nil)

// ackSession builds a session wired to the given connection with no prior
// subscription state, ready to reconcile the supplied plan.
func ackSession(t *testing.T, conn pahoConnection, subs ...connectivity.SubscriptionPlan) (*Session, connectivity.SessionPlan) {
	t.Helper()
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ack-reason",
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { s.Router().shutdown() })
	plan := connectivity.SessionPlan{Subscriptions: subs}
	s.mu.Lock()
	s.cm = conn
	s.plan = &plan
	s.mu.Unlock()
	return s, plan
}

// TestReconcile_RejectedSubscribeAckClassifiesReasonCodeNotSDKError pins that a
// SUBACK reason code of 0x87 ("not authorized") reaches the caller as the
// permanent FORBIDDEN classification even though the SDK also returned a
// generic error for it.
func TestReconcile_RejectedSubscribeAckClassifiesReasonCodeNotSDKError(t *testing.T) {
	conn := &ackAndErrorConn{
		subReasonByTopic: map[string]byte{"denied/topic": 0x87},
		subErr:           errors.New("failed to subscribe to topic: not authorized"),
	}
	s, plan := ackSession(t, conn, connectivity.SubscriptionPlan{Topic: "denied/topic", QoS: 1})

	err := s.reconcile(context.Background(), conn, plan, nil, s.connEpoch)

	var be *shared.BridgeError
	require.ErrorAs(t, err, &be)
	require.Equal(t, shared.ErrForbidden.Code, be.Code,
		"the broker's SUBACK reason code must survive the SDK error")
	require.Equal(t, shared.ErrorPermanent, be.Class)
}

// TestReconcile_RejectedSubscribeAckStillRecordsGrantedTopics pins that a mixed
// SUBACK — one grant, one denial — records the granted topic as broker-observed
// state. The SDK reports the whole call as an error, so discarding the
// acknowledgement also loses the successful grant and the cleanup knowledge
// that comes with it.
func TestReconcile_RejectedSubscribeAckStillRecordsGrantedTopics(t *testing.T) {
	conn := &ackAndErrorConn{
		subReasonByTopic: map[string]byte{"zzz/denied": 0x87},
		subErr:           errors.New("at least one requested subscription failed"),
	}
	s, plan := ackSession(t, conn,
		connectivity.SubscriptionPlan{Topic: "aaa/granted", QoS: 1},
		connectivity.SubscriptionPlan{Topic: "zzz/denied", QoS: 1},
	)

	err := s.reconcile(context.Background(), conn, plan, nil, s.connEpoch)
	require.Error(t, err)

	s.mu.Lock()
	defer s.mu.Unlock()
	require.Contains(t, s.activeSubs, "aaa/granted", "granted subscription must stay observed")
	require.NotContains(t, s.activeSubs, "zzz/denied")
}

// TestReconcile_SubscribeFailureWithoutAckKeepsSDKClassification pins the
// fallback: no reason codes means no broker verdict, so the SDK error is the
// only evidence and keeps its own classification.
func TestReconcile_SubscribeFailureWithoutAckKeepsSDKClassification(t *testing.T) {
	conn := &ackAndErrorConn{subErr: autopaho.ConnectionDownError}
	s, plan := ackSession(t, conn, connectivity.SubscriptionPlan{Topic: "t/x", QoS: 1})

	err := s.reconcile(context.Background(), conn, plan, nil, s.connEpoch)

	var be *shared.BridgeError
	require.ErrorAs(t, err, &be)
	require.Equal(t, shared.ErrConnectionLost.Code, be.Code)
}

// TestUnsubscribe_RejectedUnsubscribeAckClassifiesReasonCode pins the same rule on
// the UNSUBACK path.
func TestUnsubscribe_RejectedUnsubscribeAckClassifiesReasonCode(t *testing.T) {
	conn := &ackAndErrorConn{
		unsubReasons: []byte{0x87},
		unsubErr:     errors.New("failed to unsubscribe from topic: not authorized"),
	}
	s, _ := ackSession(t, conn)

	confirmation, err := s.unsubscribeConfirmed(context.Background(), conn, []string{"denied/topic"}, s.connEpoch)

	require.NoError(t, err, "an UNSUBACK that arrived is not an operation failure")
	require.NotNil(t, confirmation.firstErr)
	require.Equal(t, shared.ErrForbidden.Code, confirmation.firstErr.Code)
}

// TestSender_RejectedPublishAckClassifiesReasonCodeNotSDKError pins that a PUBACK
// 0x87 reaches the route as a permanent FORBIDDEN error rather than the
// transient UNAVAILABLE fallback, so the message is dead-lettered with the
// broker's cause instead of burning the replay budget.
func TestSender_RejectedPublishAckClassifiesReasonCodeNotSDKError(t *testing.T) {
	conn := &ackAndErrorConn{
		publish:    publishResult{ReasonCode: 0x87, Acknowledged: true},
		publishErr: errors.New("error publishing: Not authorized"),
	}
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ack-reason-publish",
	}, connectivity.SessionEphemeral, nil)
	sess.mu.Lock()
	sess.cm = conn
	sess.mu.Unlock()

	sender := NewSender(sess, SenderOptions{Timeout: time.Second, QoS: 1})
	err := sender.Send(context.Background(), ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "t/x", Payload: []byte("p")}),
		Address:  "denied/topic",
	})

	var be *shared.BridgeError
	require.ErrorAs(t, err, &be)
	require.Equal(t, shared.ErrForbidden.Code, be.Code)
	require.Equal(t, shared.ErrorPermanent, be.Class)
}

// TestSender_PublishFailureWithoutAckKeepsSDKClassification pins the fallback
// for a publish that never produced an acknowledgement.
func TestSender_PublishFailureWithoutAckKeepsSDKClassification(t *testing.T) {
	conn := &ackAndErrorConn{publishErr: autopaho.ConnectionDownError}
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ack-reason-publish-nolink",
	}, connectivity.SessionEphemeral, nil)
	sess.mu.Lock()
	sess.cm = conn
	sess.mu.Unlock()

	sender := NewSender(sess, SenderOptions{Timeout: time.Second, QoS: 1})
	err := sender.Send(context.Background(), ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "t/x", Payload: []byte("p")}),
		Address:  "t/x",
	})

	var be *shared.BridgeError
	require.ErrorAs(t, err, &be)
	require.Equal(t, shared.ErrConnectionLost.Code, be.Code)
}
