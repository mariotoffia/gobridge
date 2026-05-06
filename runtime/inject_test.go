package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func TestInject_SyntheticDeliveryRetry_ReturnsErrNotSupported(t *testing.T) {
	d := &syntheticDelivery{env: &messaging.Envelope{ID: "e1"}}
	secretReason := errors.New("original transport reason must not be returned")
	err := d.Retry(context.Background(), time.Second, secretReason)
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrNotSupported)
	assert.NotContains(t, err.Error(), secretReason.Error())
}
