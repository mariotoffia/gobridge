# MQTT Transport Test Index

This file indexes all tests for the MQTT transport module.

## Test Categories

| Category | Description | Location |
|----------|-------------|----------|
| Internal Unit | Tests for internal/unexported functions | `mqtt/*.go` |
| Unit | Tests for exported API | `mqtt/tests/*_test.go` |
| Simulation | Scenario-based tests | `mqtt/tests/simulation_*_test.go` |
| Integration | End-to-end tests with Mosquitto | `mqtt/tests/integration_*_test.go` |

## Internal Unit Tests

### config_internal_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| CI001 | TestSlicesEqual_BothEmpty | Empty slices are equal | PASS |
| CI002 | TestSlicesEqual_DifferentLength | Different lengths not equal | PASS |
| CI003 | TestSlicesEqual_SameContent | Same content equal | PASS |
| CI004 | TestSlicesEqual_DifferentContent | Different content not equal | PASS |
| CI005 | TestCredentialsEqual_BothNil | Nil credentials are equal | PASS |
| CI006 | TestCredentialsEqual_OneNil | One nil not equal | PASS |
| CI007 | TestCredentialsEqual_SameType | Same type equal | PASS |
| CI008 | TestGetBrokerURLs_FromBrokerURLs | Uses BrokerURLs field | PASS |
| CI009 | TestGetBrokerURLs_FallbackToBrokerURL | Falls back to BrokerURL | PASS |
| CI010 | TestGetBrokerURLs_Empty | Returns nil when empty | PASS |

### errors_internal_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| EI001 | TestContainsAny_NoMatch | No substring matches | PASS |
| EI002 | TestContainsAny_FirstMatch | First substring matches | PASS |
| EI003 | TestContainsAny_LastMatch | Last substring matches | PASS |
| EI004 | TestContainsAny_EmptyString | Empty string no match | PASS |
| EI005 | TestContainsAny_EmptySubstrings | Empty substrings no match | PASS |

### source_internal_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| SI001 | TestSource_HandleMessage_NotRunning | Drops message when not running | PASS |
| SI002 | TestSource_HandleMessage_ExtractsProperties | Extracts MQTT v5 properties | PASS |
| SI003 | TestSource_HandleMessage_ChannelFull | Handles full channel gracefully | PASS |

### target_internal_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| TI001 | TestTarget_ShouldUseTransportRetry_QoS0 | QoS 0 uses transport retry | PASS |
| TI002 | TestTarget_ShouldUseTransportRetry_QoS1 | QoS 1 skips transport retry | PASS |
| TI003 | TestTarget_ShouldUseTransportRetry_QoS2 | QoS 2 skips transport retry | PASS |
| TI004 | TestTarget_ShouldUseTransportRetry_ForceEnabled | SkipNativeRetry=false forces retry | PASS |
| TI005 | TestTarget_HasNativeRetry_QoS0 | QoS 0 has no native retry | PASS |
| TI006 | TestTarget_HasNativeRetry_QoS1 | QoS 1 has native retry | PASS |
| TI007 | TestTarget_HasNativeRetry_QoS2 | QoS 2 has native retry | PASS |

## Unit Tests (mqtt/tests/)

### config_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| C001 | TestSourceConfig_RequiresBrokerURL | BrokerURL required | PASS |
| C002 | TestSourceConfig_RequiresTopics | Topics required | PASS |
| C003 | TestTargetConfig_RequiresBrokerURL | BrokerURL required | PASS |
| C004 | TestQoS_ClampingAboveTwo | QoS > 2 clamps to 1 | PASS |
| C005 | TestMQTTConnectionSettings_Builder | Fluent API | PASS |
| C006 | TestRequiresReconnect_BrokerURLChange | Detects URL change | PASS |
| C007 | TestRequiresReconnect_ClientIDChange | Detects ClientID change | PASS |
| C008 | TestRequiresReconnect_CredentialsChange | Detects creds change | PASS |
| C009 | TestRequiresReconnect_TLSChange | Detects TLS change | PASS |
| C010 | TestRequiresReconnect_NoChange | Same config returns false | PASS |

### source_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| S001 | TestNewSource_NilConfig | Returns error | PASS |
| S002 | TestNewSource_NoBroker | Returns error | PASS |
| S003 | TestNewSource_NoTopics | Returns error | PASS |
| S004 | TestNewSource_ValidConfig | Success | PASS |
| S005 | TestNewSourceWithClient_NilClient | Returns error | PASS |
| S006 | TestNewSourceWithClient_NilRouter | Returns error | PASS |
| S007 | TestSource_GetID | Returns configured ID | PASS |
| S008 | TestSource_GetTransportType | Returns MQTT | PASS |
| S009 | TestSource_Capabilities_QoS0 | ReceiveAtMostOnce | PASS |
| S010 | TestSource_Capabilities_QoS1 | ReceiveAtLeastOnce | PASS |
| S011 | TestSource_Capabilities_QoS2 | ReceiveExactOnce | PASS |
| S012 | TestSource_Messages | Returns non-nil channel | PASS |
| S013 | TestSource_Close_Idempotent | Multiple calls safe | PASS |

### target_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| T001 | TestNewTarget_NilConfig | Returns error | PASS |
| T002 | TestNewTarget_NoBroker | Returns error | PASS |
| T003 | TestNewTarget_ValidConfig | Success | PASS |
| T004 | TestNewTargetWithClient_NilClient | Returns error | PASS |
| T005 | TestTarget_GetID | Returns configured ID | PASS |
| T006 | TestTarget_GetTransportType | Returns MQTT | PASS |
| T007 | TestTarget_Capabilities_QoS0 | PublishAtMostOnce | PASS |
| T008 | TestTarget_Capabilities_QoS1 | PublishAtLeastOnce + NativeRetry | PASS |
| T009 | TestTarget_Capabilities_QoS2 | PublishExactOnce + NativeRetry | PASS |
| T010 | TestTarget_DefaultTimeout | 30s when not specified | PASS |
| T011 | TestTarget_CustomTimeout | Honors configured value | PASS |
| T012 | TestTarget_Close_Idempotent | Multiple calls safe | PASS |
| T013 | TestWithTransportRetry | Configures retry | PASS |
| T014 | TestWithDefaultTTL | Configures TTL | PASS |

### connection_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| CN001 | TestNewConnection_NilConfig | Returns error | PASS |
| CN002 | TestNewConnection_NoBroker | Returns error | PASS |
| CN003 | TestNewConnection_ValidConfig | Success | PASS |
| CN004 | TestConnection_GetID | Returns configured ID | PASS |
| CN005 | TestConnection_GetTransportType | Returns MQTT | PASS |
| CN006 | TestConnection_Capabilities | All QoS + NativeRetry | PASS |
| CN007 | TestConnection_CreateSourceBeforeStart | Returns error | PASS |
| CN008 | TestConnection_CreateTargetBeforeStart | Returns error | PASS |
| CN009 | TestConnection_IsRunning | Correct state tracking | PASS |
| CN010 | TestConnection_IsDraining | Correct drain state | PASS |

### factory_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| F001 | TestSourceFactory_SupportedTransports | Returns MQTT | PASS |
| F002 | TestSourceFactory_CreateSource_WrongType | Returns error | PASS |
| F003 | TestTargetFactory_SupportedTransports | Returns MQTT | PASS |
| F004 | TestTargetFactory_CreateTarget_WrongType | Returns error | PASS |

### errors_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| E001 | TestMapError_Nil | Returns nil | PASS |
| E002 | TestMapError_DeadlineExceeded | ErrTimeout | PASS |
| E003 | TestMapError_Canceled | ErrUnavailable | PASS |
| E004 | TestMapError_NetTimeout | ErrTimeout | PASS |
| E005 | TestMapError_NetOther | ErrConnectionLost | PASS |
| E006 | TestMapError_ConnectionRefused | ErrConnectionLost | PASS |
| E007 | TestMapError_ServerUnavailable | ErrUnavailable | PASS |
| E008 | TestMapPublishError_Success | nil | PASS |
| E009 | TestMapPublishError_NoSubscribers | nil | PASS |
| E010 | TestMapPublishError_NotAuthorized | ErrForbidden | PASS |
| E011 | TestMapPublishError_InvalidTopic | ErrInvalidTopic | PASS |
| E012 | TestMapPublishError_QuotaExceeded | ErrThrottled | PASS |
| E013 | TestMapDisconnect_ServerBusy | ErrBrokerBusy | PASS |
| E014 | TestMapDisconnect_KeepAliveTimeout | ErrTimeout | PASS |
| E015 | TestMapDisconnect_SessionTakenOver | ErrConnectionLost | PASS |
| E016 | TestMapDisconnect_MalformedPacket | ErrProtocolError | PASS |
| E017 | TestMapDisconnect_BadCredentials | ErrNotAuthorized | PASS |
| E018 | TestMapDisconnect_TopicInvalid | ErrInvalidTopic | PASS |
| E019 | TestMapDisconnect_PacketTooLarge | ErrPayloadTooLarge | PASS |
| E020 | TestErrorClassification | Recoverable/Permanent accuracy | PASS |

### lifecycle_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| L001 | TestBeginTransaction | Returns transaction | PASS |
| L002 | TestTransaction_AddSource | Schedules add | PASS |
| L003 | TestTransaction_RemoveSource | Schedules remove | PASS |
| L004 | TestTransaction_UpdateSource | Schedules update | PASS |
| L005 | TestTransaction_AddTarget | Schedules add | PASS |
| L006 | TestTransaction_RemoveTarget | Schedules remove | PASS |
| L007 | TestTransaction_UpdateTarget | Schedules update | PASS |
| L008 | TestTransaction_CommitAfterCommit | Returns error | PASS |
| L009 | TestTransaction_CommitAfterRollback | Returns error | PASS |
| L010 | TestTransaction_RollbackAfterCommit | Returns error | PASS |
| L011 | TestTransaction_RollbackReleasesLock | Releases lock | PASS |

## Simulation Tests

### simulation_qos_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| Q001 | TestQoS0_PublishSuccess | Fire-and-forget | PASS |
| Q002 | TestQoS0_ConnectionDrop | Message lost silently | PASS |
| Q003 | TestQoS1_PublishSuccess | PUBACK received | PASS |
| Q004 | TestQoS1_PUBACKTimeout | Transport retry | PASS |
| Q005 | TestQoS1_PUBACKDelayed | Wait completes | PASS |
| Q006 | TestQoS1_DuplicatePUBACK | Handled gracefully | PASS |
| Q007 | TestQoS2_PublishSuccess | Full handshake | PASS |
| Q008 | TestQoS2_PUBRECTimeout | Retry or error | PASS |
| Q009 | TestQoS2_PUBRELSent | Mid-handshake | PASS |
| Q010 | TestQoS2_PUBCOMPReceived | Transaction complete | PASS |
| Q011 | TestSubscribe_QoS0 | At-most-once receive | PASS |
| Q012 | TestSubscribe_QoS1 | At-least-once receive | PASS |
| Q013 | TestSubscribe_QoS2 | Exactly-once receive | PASS |
| Q014 | TestSubscribe_Downgrade | Broker grants lower | PASS |
| Q015 | TestMixedQoS_Sources | Multiple QoS | PASS |

### simulation_subscription_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| SUB001 | TestSingleTopicSubscribe | Basic subscription | PASS |
| SUB002 | TestMultipleTopicsSubscribe | Batch subscription | PASS |
| SUB003 | TestWildcardPlus | Single-level wildcard | PASS |
| SUB004 | TestWildcardHash | Multi-level wildcard | PASS |
| SUB005 | TestDynamicAddSubscription | Add on fly | PASS |
| SUB006 | TestDynamicRemoveSubscription | Remove on fly | PASS |
| SUB007 | TestUpdateSubscriptionQoS | Change QoS | PASS |
| SUB008 | TestSubscriptionTimeout | Slow response | PASS |
| SUB009 | TestSubscriptionDenied | ACL rejection | PASS |
| SUB010 | TestResubscribeOnReconnect | Session restore | PASS |
| SUB011 | TestCleanSession | No persistence | PASS |
| SUB012 | TestPersistentSession | Offline queue | PASS |
| SUB013 | TestSharedSubscription | Multi-consumer | PASS |
| SUB014 | TestOverlappingWildcards | Topic matching | PASS |
| SUB015 | TestTransaction_AddSource | Atomic add | PASS |
| SUB016 | TestTransaction_RemoveSource | Atomic remove | PASS |
| SUB017 | TestTransaction_Rollback | Changes reverted | PASS |

### simulation_publish_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| PUB001 | TestPublishToTopic | Basic publish | PASS |
| PUB002 | TestPublishRetain | Retained message | PASS |
| PUB003 | TestPublishMessageExpiry | TTL on message | PASS |
| PUB004 | TestPublishUserProperties | MQTT v5 props | PASS |
| PUB005 | TestPublishCorrelationData | Request-response | PASS |
| PUB006 | TestPublishResponseTopic | Reply-to pattern | PASS |
| PUB007 | TestChangeTargetTopic | Dynamic topic | PASS |
| PUB008 | TestChangeTargetQoS | Dynamic QoS | PASS |
| PUB009 | TestPublishTimeout | Slow broker | PASS |
| PUB010 | TestPublishQuotaExceeded | Rate limiting | PASS |
| PUB011 | TestPublishTopicInvalid | Bad topic | PASS |
| PUB012 | TestPublishPayloadTooLarge | Size limit | PASS |
| PUB013 | TestPublishNotAuthorized | ACL rejection | PASS |
| PUB014 | TestBatchPublish | Sequential sends | PASS |
| PUB015 | TestConcurrentPublish | Parallel sends | PASS |
| PUB016 | TestTransaction_AddTarget | Atomic add | PASS |
| PUB017 | TestTransaction_UpdateTarget | Config change | PASS |

### simulation_retry_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| R001 | TestQoS0_TransportRetry | Retry enabled | PASS |
| R002 | TestQoS1_SkipTransportRetry | Native retry | PASS |
| R003 | TestQoS2_SkipTransportRetry | Native retry | PASS |
| R004 | TestRetryBackoffCalculation | Exponential | PASS |
| R005 | TestRetryJitter | Randomness | PASS |
| R006 | TestRetryInfrastructureBackoff | 2x multiplier | PASS |
| R007 | TestRetryTTLExpiration | Stops when expired | PASS |
| R008 | TestRetrySuccessAfterFailures | Eventually succeeds | PASS |
| R009 | TestRetryMaxBackoffCap | Doesn't exceed | PASS |
| R010 | TestRetryContextCancellation | Respects ctx | PASS |
| R011 | TestSkipNativeRetryFalse | Force retry | PASS |
| R012 | TestMessageCreatedAtRespected | TTL from creation | PASS |

## Integration Tests

### integration_mqtt_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| I001 | TestIntegration_Source_ReceiveMessages | End-to-end receive | PASS |
| I002 | TestIntegration_Target_SendMessages | End-to-end send | PASS |
| I003 | TestIntegration_RoundTrip | Full pub/sub | PASS |
| I004 | TestIntegration_QoS0_Delivery | Fire-and-forget | PASS |
| I005 | TestIntegration_QoS1_Delivery | At-least-once | PASS |
| I006 | TestIntegration_QoS2_Delivery | Exactly-once | PASS |
| I007 | TestIntegration_WildcardSubscription | Pattern match | PASS |
| I008 | TestIntegration_RetainedMessages | Persistence | PASS |
| I009 | TestIntegration_MessageProperties | MQTT v5 | PASS |
| I010 | TestIntegration_SharedConnection | Multiple src/tgt | PASS |
| I011 | TestIntegration_StandaloneConnection | Independent | PASS |
| I012 | TestIntegration_GracefulShutdown | Context cancel | PASS |
| I013 | TestIntegration_ReconnectionOnDrop | Auto-recovery | PASS |
| I014 | TestIntegration_CleanSession | Session state | PASS |
| I015 | TestIntegration_PersistentSession | Offline queue | PASS |

### error_injection_test.go

| ID | Name | Description | Status |
|----|------|-------------|--------|
| EI001 | TestErrorInjection_ConnectionRefused | Broker down | PASS |
| EI002 | TestErrorInjection_ConnectionTimeout | Slow broker | PASS |
| EI003 | TestErrorInjection_AuthFailure | Bad creds | PASS |
| EI004 | TestErrorInjection_AuthorizationDenied | ACL reject | PASS |
| EI005 | TestErrorInjection_TopicNotFound | Invalid topic | PASS |
| EI006 | TestErrorInjection_PacketTooLarge | Size limit | PASS |
| EI007 | TestErrorInjection_QuotaExceeded | Rate limit | PASS |
| EI008 | TestErrorInjection_ServerBusy | Overloaded | PASS |
| EI009 | TestErrorInjection_SessionTakenOver | Dup client | PASS |
| EI010 | TestErrorInjection_KeepAliveTimeout | Network split | PASS |
| EI011 | TestErrorInjection_ProtocolError | Malformed | PASS |
| EI012 | TestErrorInjection_RecoveryAfterError | Transient | PASS |

---

## Test Summary

| Category | File Count | Test Count | Status |
|----------|------------|------------|--------|
| Internal Unit | 4 | 25 | ✅ PASS |
| Unit Tests | 7 | 75 | ✅ PASS |
| Simulation | 4 | 35 | ✅ PASS (integration) |
| Integration | 2 | 22 | ✅ PASS (integration) |
| **Total** | **17** | **157** | **✅ ALL PASS** |

## Running Tests

```bash
# Run all tests (requires Docker for simulation/integration tests)
cd transport/mqtt
go test -v . ./tests/...
```

---

**Total Tests: 157**

Last Updated: 2024-12-16
