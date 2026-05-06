package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInject_SyntheticDeliveryRetry_ReturnsErrNotSupported(t *testing.T) {
	d := &syntheticDelivery{env: &domain.Envelope{ID: "e1"}}
	secretReason := errors.New("original transport reason must not be returned")
	err := d.Retry(context.Background(), time.Second, secretReason)
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrNotSupported)
	assert.NotContains(t, err.Error(), secretReason.Error())
}
