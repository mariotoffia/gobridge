package transporttest_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/ports/transporttest"
)

// The reference transport in transporttest must PASS every suite the kit
// exports, under every relevant capability combination. These self-tests are
// what make the kit trustworthy: if a suite is wrong, or the reference drifts
// from the canonical contract, one of these fails.

func TestReferenceDelivery_FullCaps(t *testing.T) {
	caps := transporttest.Caps{SupportsRetry: true, SupportsExtend: true}
	transporttest.RunDeliveryConformanceTests(t, transporttest.ReferenceDeliveryFactory(caps), caps)
}

func TestReferenceDelivery_NoOptionalCaps(t *testing.T) {
	// Exercises the ErrNotSupported / no-latch paths for Retry and Extend.
	caps := transporttest.Caps{}
	transporttest.RunDeliveryConformanceTests(t, transporttest.ReferenceDeliveryFactory(caps), caps)
}

func TestReferenceDelivery_RetryOnly(t *testing.T) {
	caps := transporttest.Caps{SupportsRetry: true}
	transporttest.RunDeliveryConformanceTests(t, transporttest.ReferenceDeliveryFactory(caps), caps)
}

func TestReferenceReceiver_FullCaps(t *testing.T) {
	caps := transporttest.Caps{SupportsRetry: true, SupportsExtend: true}
	transporttest.RunReceiverConformanceTests(t, transporttest.ReferenceReceiverFactory(caps), caps)
}

func TestReferenceReceiver_NoOptionalCaps(t *testing.T) {
	caps := transporttest.Caps{}
	transporttest.RunReceiverConformanceTests(t, transporttest.ReferenceReceiverFactory(caps), caps)
}

func TestReferenceSender_WithBatch(t *testing.T) {
	caps := transporttest.Caps{SupportsBatchSend: true}
	transporttest.RunSenderConformanceTests(t, transporttest.ReferenceSenderFactory(caps), caps)
}

func TestReferenceSender_NoBatch(t *testing.T) {
	caps := transporttest.Caps{}
	transporttest.RunSenderConformanceTests(t, transporttest.ReferenceSenderFactory(caps), caps)
}
