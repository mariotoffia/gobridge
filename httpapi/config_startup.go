package httpapi

import (
	"context"
	"errors"
	"net"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ConfigStartupPending identifies a clean wait or a positively classified
// retryable startup fault. Unknown errors, validation and authorization fail
// closed: a deployment probe must not hide a rejected configuration as waiting.
func ConfigStartupPending(err error) bool {
	if err == nil {
		return true
	}
	var validation *ports.BlueprintValidationError
	if errors.As(err, &validation) || errors.Is(err, shared.ErrInvalidConfig) || errors.Is(err, shared.ErrNotAuthorized) {
		return false
	}
	var bridgeErr *shared.BridgeError
	if errors.As(err, &bridgeErr) {
		return bridgeErr != nil && bridgeErr.Class == shared.ErrorTransient
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return true
	}
	var response interface{ HTTPStatusCode() int }
	if errors.As(err, &response) {
		code := response.HTTPStatusCode()
		return code == 429 || (code >= 500 && code < 600)
	}
	return false
}
