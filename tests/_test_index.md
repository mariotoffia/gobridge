# Test Index

This file indexes all tests in the gobridge project.

## Test Summary

| Name | Description | Type | Group | Status |
|------|-------------|------|-------|--------|
| TestFileConfigItem_* | fileConfigItem interface implementation | unit | config/file | ✓ |
| TestFormatConstants | Format string constants validation | unit | config/file | ✓ |
| TestParser_* | Parser YAML/JSON parsing and format detection | unit | config/file | ✓ |
| TestToConfigItems_* | FileConfig to ConfigItem conversion | unit | config/file | ✓ |
| TestComputeChanges_* | Change detection (add/update/delete) | unit | config/file | ✓ |
| TestNewConfigSource_* | FileConfigSource creation | unit | config/file | ✓ |
| TestFileConfigSource_* | FileConfigSource operations | unit | config/file | ✓ |
| TestNewFileWatcher | FileWatcher creation | unit | config/file | ✓ |
| TestFileWatcher_* | FileWatcher lifecycle operations | unit | config/file | ✓ |
| TestIntegration_FileConfigSource_* | Full lifecycle with real files | integration | config/file | ✓ |
| TestGenerateCorrelationID | Correlation ID generation | unit | logging | ✓ |
| TestWithCorrelationID | Correlation ID context handling | unit | logging | ✓ |
| TestExtractOrGenerateCorrelationID | Correlation ID extraction | unit | logging | ✓ |
| TestInjectCorrelationID | Correlation ID metadata injection | unit | logging | ✓ |
| TestGenerateTraceID | Trace ID generation | unit | logging | ✓ |
| TestGenerateSpanID | Span ID generation | unit | logging | ✓ |
| TestWithTraceID_GetTraceID | Trace ID context handling | unit | logging | ✓ |
| TestWithSpanID_GetSpanID | Span ID context handling | unit | logging | ✓ |
| TestLogContext_* | LogContext struct operations | unit | logging | ✓ |
| TestNewContextLogger | ContextLogger creation | unit | logging | ✓ |
| TestContextLogger_* | ContextLogger operations | unit | logging | ✓ |
| TestLoggerFromContext | ContextLogger factory | unit | logging | ✓ |
| TestLoggerWithIDs | Logger with explicit IDs | unit | logging | ✓ |
| TestHTTPCorrelationMiddleware_* | HTTP middleware ID handling | unit | logging | ✓ |
| TestExtractCorrelationIDFromHeaders | Header extraction | unit | logging | ✓ |
| TestExtractTraceContextFromHeaders_* | W3C TraceContext parsing | unit | logging | ✓ |
| TestInjectTraceContext* | Header injection | unit | logging | ✓ |
| TestRoundTripperWithCorrelation | HTTP client round tripper | unit | logging | ✓ |
| TestNewHTTPClientWithCorrelation | HTTP client factory | unit | logging | ✓ |

## File Locations

| File | Package | LOC |
|------|---------|-----|
| bridge/config/file/types_test.go | file | 124 |
| bridge/config/file/parser_test.go | file | 606 |
| bridge/config/file/source_test.go | file | 465 |
| bridge/config/file/watcher_test.go | file | 148 |
| bridge/config/file/integration_file_source_test.go | file | 462 |
| bridge/middleware/transport/logging/correlation_test.go | logging | 553 |
| bridge/middleware/transport/logging/context_logger_test.go | logging | 466 |
| bridge/middleware/transport/logging/http_test.go | logging | 535 |

**Total: 3,359 LOC**

## Package-Specific Test Indexes

Tests for specific transport packages are maintained in their own directories:

- [transport/aws/sqs/tests/_test_index.md](../transport/aws/sqs/tests/_test_index.md) - SQS transport tests (2,229 LOC)
