# Azure Service Bus Transport Test Index

This file indexes all tests for the Azure Service Bus transport package.

## Test Summary

| Name | Description | Type | Group | Status |
|------|-------------|------|-------|--------|
| TestTransportType_Constant | TransportType constant validation | unit | config | ✓ |
| TestSourceConfigImpl_* | SourceConfigImpl interface implementation | unit | config | ✓ |
| TestTargetConfigImpl_* | TargetConfigImpl interface implementation | unit | config | ✓ |
| TestNewSource_* | Service Bus Source constructor validation | unit | source | ✓ |
| TestSource_* | Service Bus Source behavior and capabilities | unit | source | ✓ |
| TestNewTarget_* | Service Bus Target constructor validation | unit | target | ✓ |
| TestTarget_* | Service Bus Target behavior and capabilities | unit | target | ✓ |
| TestBuildMessage_* | Service Bus message building (internal) | unit | target-internal | ✓ |
| TestConvertMessage_* | Service Bus to bridge message conversion (internal) | unit | source-internal | ✓ |
| TestSourceFactory_* | Service Bus SourceFactory implementation | unit | factory | ✓ |
| TestTargetFactory_* | Service Bus TargetFactory implementation | unit | factory | ✓ |
| TestMapError_* | Azure Service Bus error mapping to BridgeError | unit | errors | ✓ |
| TestContainsAny_* | String matching helper | unit | errors | ✓ |
| TestIntegration_ServiceBus_Source_* | Service Bus Source integration with Artemis | integration | source | ✓ |
| TestIntegration_ServiceBus_Target_* | Service Bus Target integration with Artemis | integration | target | ✓ |
| TestIntegration_ServiceBus_RoundTrip | Full Service Bus message flow integration | integration | e2e | ✓ |

## File Locations

| File | Package | LOC | Description |
|------|---------|-----|-------------|
| config_test.go | servicebustests | ~180 | Configuration tests |
| source_test.go | servicebustests | ~250 | Source unit tests |
| target_test.go | servicebustests | ~220 | Target unit tests |
| factory_test.go | servicebustests | ~130 | Factory tests |
| errors_test.go | servicebustests | ~350 | Error mapping tests |
| servicebus_local.go | servicebustests | ~100 | Artemis helper |
| integration_servicebus_test.go | servicebustests | ~500 | Integration tests |

### Internal Tests (package servicebus)

| File | Package | LOC | Description |
|------|---------|-----|-------------|
| target_internal_test.go | servicebus | ~320 | buildMessage |
| source_internal_test.go | servicebus | ~280 | convertMessage |

**Total: ~2,330 LOC**

## Running Tests

```bash
# Unit tests only (no Docker required)
go test ./transport/azure/servicebus/... -v

# Internal tests only (unexported functions)
go test ./transport/azure/servicebus -v

# External tests only (exported API)
go test ./transport/azure/servicebus/tests/... -v

# Integration tests (Docker with Artemis required)
go test -tags=integration ./transport/azure/servicebus/tests/... -v
```

## Integration Test Environment

Integration tests use Apache ActiveMQ Artemis as an Azure Service Bus compatible test environment.
Artemis supports AMQP 1.0 which the Azure Service Bus SDK uses.

### Docker Container

The tests use the `tests/docker/azure_service_bus.go` helper which provides:
- ArtemisContainer with AMQP and console ports
- Queue and topic creation
- Azure Service Bus compatible connection strings

### Known Differences from Azure Service Bus

- Sessions: No strict FIFO guarantee
- Lock renewal: Different timeout behavior
- Dead-letter reasons: Metadata differs
- Scheduled enqueue: Lower precision

## Test Categories

### Configuration Tests (config_test.go)

Tests for SourceConfigImpl and TargetConfigImpl:
- Interface compliance with types.SourceConfig/TargetConfig
- Getter methods (GetID, GetTransportType, GetQoS, etc.)
- Default value handling

### Source Tests (source_test.go)

Tests for Source constructor and behavior:
- Config validation (nil, missing queue/topic, missing connection)
- Default value application
- Capability reporting

### Target Tests (target_test.go)

Tests for Target constructor and behavior:
- Config validation
- Default value application
- Capability reporting

### Factory Tests (factory_test.go)

Tests for SourceFactory and TargetFactory:
- Interface compliance
- SupportedTransports()
- CreateSource/CreateTarget with invalid configs

### Error Tests (errors_test.go)

Tests for MapError and error classification:
- Context errors (deadline, canceled)
- Azure Service Bus specific errors
- String pattern matching for error messages
- Recoverable vs permanent error classification

### Internal Tests

#### source_internal_test.go
Tests convertMessage function:
- Basic message conversion
- Property extraction (MessageID, CorrelationID, SessionID, etc.)
- ApplicationProperties mapping
- Topic determination

#### target_internal_test.go
Tests buildMessage function:
- Payload and subject setting
- SessionID handling (default and override)
- TTL, MessageID, CorrelationID mapping
- ApplicationProperties filtering

### Integration Tests (integration_servicebus_test.go)

End-to-end tests with Artemis:
- Source receiving messages
- Source Ack/Nack operations
- Target sending messages
- Batch operations
- Full round-trip flow
- Topic/subscription pub-sub
- Graceful shutdown
