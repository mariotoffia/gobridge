# SQS Transport Test Index

This file indexes all tests for the SQS transport package.

## Test Summary

| Name | Description | Type | Group | Status |
|------|-------------|------|-------|--------|
| TestTransportType_Constant | TransportType constant validation | unit | config | ✓ |
| TestSourceConfigImpl_* | SourceConfigImpl interface implementation | unit | config | ✓ |
| TestTargetConfigImpl_* | TargetConfigImpl interface implementation | unit | config | ✓ |
| TestNewSource_* | SQS Source constructor validation | unit | source | ✓ |
| TestSource_* | SQS Source behavior and capabilities | unit | source | ✓ |
| TestNewTarget_* | SQS Target constructor validation | unit | target | ✓ |
| TestTarget_* | SQS Target behavior and capabilities | unit | target | ✓ |
| TestBuildMessageAttributes_* | SQS message attribute conversion | unit | target | ✓ |
| TestGenerateDeduplicationID | FIFO deduplication ID generation | unit | target | ✓ |
| TestSourceFactory_* | SQS SourceFactory implementation | unit | factory | ✓ |
| TestTargetFactory_* | SQS TargetFactory implementation | unit | factory | ✓ |
| TestMapError_* | AWS/SQS error mapping to BridgeError | unit | errors | ✓ |
| TestContainsAny_* | String matching helper | unit | errors | ✓ |
| TestIntegration_SQS_Source_* | SQS Source integration with LocalStack | integration | source | ✓ |
| TestIntegration_SQS_Target_* | SQS Target integration with LocalStack | integration | target | ✓ |
| TestIntegration_SQS_FIFO_* | FIFO queue integration tests | integration | fifo | ✓ |
| TestIntegration_SQS_RoundTrip | Full SQS message flow integration | integration | e2e | ✓ |
| TestRoundTripper_QueueMode | RoundTripper LIFO queue behavior | integration | error-injection | ✓ |
| TestRoundTripper_LatchMode | RoundTripper latch mode behavior | integration | error-injection | ✓ |
| TestTarget_ErrorInjection_* | Target error injection tests | integration | error-injection | ✓ |
| TestErrorClassification | Error type classification validation | integration | error-injection | ✓ |

## File Locations

| File | Package | LOC | Description |
|------|---------|-----|-------------|
| config_test.go | sqstests | 194 | Configuration tests |
| source_test.go | sqstests | 302 | Source unit tests |
| target_test.go | sqstests | 301 | Target unit tests |
| factory_test.go | sqstests | 162 | Factory tests |
| errors_test.go | sqstests | 388 | Error mapping tests |
| sqs_local.go | sqstests | 265 | LocalStack helper |
| integration_sqs_test.go | sqstests | 657 | Integration tests |
| error_injection_test.go | sqstests | 580 | Error injection tests |

**Total: 2,849 LOC**

## Shared Test Utilities

The roundtripper utilities have been moved to `tests/awsutils/` module for reuse across AWS transports:

| File | Package | LOC | Description |
|------|---------|-----|-------------|
| roundtripper.go | awsutils | 235 | HTTP RoundTripper for error injection |
| sqs.go | awsutils | 260 | SQS-specific error helpers |

## Running Tests

```bash
# Unit tests only (no Docker required)
go test ./transport/aws/sqs/tests/... -v

# Integration tests (Docker with LocalStack required)
go test -tags=integration ./transport/aws/sqs/tests/... -v

# Error injection tests only
go test -tags=integration ./transport/aws/sqs/tests/... -v -run "ErrorInjection|RoundTripper|ErrorClassification"
```

## Error Injection Testing

The `tests/awsutils` module provides utilities for injecting AWS errors in tests
without requiring network failures. Import with:

```go
import "github.com/mariotoffia/gobridge/tests/awsutils"
```

### Available Error Types

**Retryable Errors:**
- `awsutils.SqsErrors{}.OverLimit()` - Throttling (OverLimit)
- `awsutils.SqsErrors{}.ServiceUnavailable()` - Service unavailable (503)
- `awsutils.SqsErrors{}.InternalError()` - Internal server error (500)
- `awsutils.SqsErrors{}.ThrottlingException()` - Rate limiting
- `awsutils.SqsErrors{}.RequestThrottled()` - Account-level throttling
- `awsutils.SqsErrors{}.NetworkError()` - Connection refused
- `awsutils.SqsErrors{}.ConnectionReset()` - Connection reset by peer
- `awsutils.SqsErrors{}.Timeout()` - Request timeout

**Non-Retryable Errors:**
- `awsutils.SqsErrors{}.QueueDoesNotExist()` - Queue not found
- `awsutils.SqsErrors{}.InvalidMessageContents()` - Invalid message
- `awsutils.SqsErrors{}.AccessDenied()` - Access denied
- `awsutils.SqsErrors{}.BatchRequestTooLong()` - Batch too large

### Usage Example

```go
import "github.com/mariotoffia/gobridge/tests/awsutils"

func TestWithErrorInjection(t *testing.T) {
    helper, cleanup := setupSQSTest(t)
    defer cleanup()

    // Inject 2 throttling errors, then allow pass-through
    helper.RoundTripper().
        Enable().
        PushN(awsutils.SqsErrors{}.OverLimit(), 2)

    // First 2 sends will fail with throttling
    // Third send will pass through to LocalStack
}
```
