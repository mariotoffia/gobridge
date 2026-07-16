# Test Index

Catalog of all test functions in the gobridge repository.

| name | description | type | group | status |
|------|-------------|------|-------|--------|
| TestEnvelope_HasExpiry | validates envelope expiry detection | unit | domain | pass |
| TestEnvelope_IsExpired | validates envelope expired check | unit | domain | pass |
| TestEnvelope_RemainingTTL | validates envelope remaining TTL calculation | unit | domain | pass |
| TestEnvelope_Clone | validates envelope deep clone | unit | domain | pass |
| TestEnvelope_Clone_NilFields | validates envelope clone with nil fields | unit | domain | pass |
| TestRoutePolicy_WithDefaults | validates route policy default values | unit | domain | pass |
| TestRoutePolicy_WithDefaults_PreservesExplicit | validates explicit values preserved after defaults | unit | domain | pass |
| TestFixedPoll_NextInterval | validates fixed poll interval calculation | unit | domain | pass |
| TestFixedPoll_DefaultInterval | validates fixed poll default interval | unit | domain | pass |
| TestFixedPoll_Stable | validates fixed poll stability across calls | unit | domain | pass |
| TestAdaptiveBackoff_ResetOnRecords | validates adaptive backoff resets on records | unit | domain | pass |
| TestAdaptiveBackoff_BackoffOnEmpty | validates adaptive backoff increases on empty | unit | domain | pass |
| TestAdaptiveBackoff_CapsAtMax | validates adaptive backoff caps at maximum | unit | domain | pass |
| TestAdaptiveBackoff_FullRamp | validates adaptive backoff full ramp sequence | unit | domain | pass |
| TestAdaptiveBackoff_ConstructorDefaults | validates adaptive backoff default construction | unit | domain | pass |
| TestAdaptiveBackoff_MaxClamped | validates adaptive backoff max clamping | unit | domain | pass |
| TestAdaptiveBackoff_MultiplierFloor | validates adaptive backoff multiplier minimum | unit | domain | pass |
| TestAdaptiveBackoff_Reset | validates adaptive backoff manual reset | unit | domain | pass |
| TestBridgeError_Error | validates bridge error message formatting | unit | domain | pass |
| TestBridgeError_Is | validates bridge error identity comparison | unit | domain | pass |
| TestBridgeError_As | validates bridge error type assertion | unit | domain | pass |
| TestBridgeError_With | validates bridge error with additional context | unit | domain | pass |
| TestBridgeError_WithMessage | validates bridge error message override | unit | domain | pass |
| TestBridgeError_Unwrap | validates bridge error unwrapping | unit | domain | pass |
| TestIsRecoverableError | validates recoverable error detection | unit | domain | pass |
| TestGetRetryAfter | validates retry-after extraction from errors | unit | domain | pass |
| TestNewBridgeError | validates bridge error construction | unit | domain | pass |
| TestSentinelClasses | validates sentinel error class constants | unit | domain | pass |
| TestErrMessageFiltered_Is | validates filtered message error identity | unit | domain | pass |
| TestIsReservedHeader | validates reserved header detection | unit | domain | pass |
| TestStripReservedHeaders | validates reserved header stripping | unit | domain | pass |
| TestStripReservedHeaders_Nil | validates nil header stripping | unit | domain | pass |
| TestMergeHeaders | validates header map merging | unit | domain | pass |
| TestMergeHeaders_NilInputs | validates merge with nil inputs | unit | domain | pass |
| TestGetHeaderString | validates header string extraction | unit | domain | pass |
| TestSetHeader | validates header value setting | unit | domain | pass |
| TestParseTraceparent | validates W3C traceparent parsing | unit | domain | pass |
| TestFormatTraceparent | validates W3C traceparent formatting | unit | domain | pass |
| TestExtractTraceContext | validates trace context extraction from headers | unit | domain | pass |
| TestInjectTraceContext | validates trace context injection into headers | unit | domain | pass |
| TestParse_YAML | validates YAML config parsing | unit | config | pass |
| TestParse_JSON | validates JSON config parsing | unit | config | pass |
| TestParse_InvalidYAML | validates invalid YAML rejection | unit | config | pass |
| TestParse_InvalidJSON | validates invalid JSON rejection | unit | config | pass |
| TestDetectFormat | validates config format detection | unit | config | pass |
| TestBridgeSettings_Durations | validates bridge settings duration parsing | unit | config | pass |
| TestBridgeSettings_DurationDefaults | validates bridge settings duration defaults | unit | config | pass |
| TestValidate_ValidConfig | validates valid config acceptance | unit | config | pass |
| TestValidate_MissingBridgeID | validates missing bridge ID rejection | unit | config | pass |
| TestValidate_DuplicateSessionIDs | validates duplicate session ID rejection | unit | config | pass |
| TestValidate_InvalidSessionMode | validates invalid session mode rejection | unit | config | pass |
| TestValidate_ReceiverMissingTransport | validates receiver missing transport rejection | unit | config | pass |
| TestValidate_ReceiverBadSessionRef | validates bad session reference rejection | unit | config | pass |
| TestValidate_BindingMissingSender | validates binding missing sender rejection | unit | config | pass |
| TestValidate_BindingBadSenderRef | validates bad sender reference rejection | unit | config | pass |
| TestValidate_RouteMissingReceiver | validates route missing receiver rejection | unit | config | pass |
| TestValidate_RouteBadReceiverRef | validates bad receiver reference rejection | unit | config | pass |
| TestValidate_InvalidDeliveryMode | validates invalid delivery mode rejection | unit | config | pass |
| TestValidate_InvalidDispatchMode | validates invalid dispatch mode rejection | unit | config | pass |
| TestValidate_SharedOutboxWithoutStore | validates shared outbox without store rejection | unit | config | pass |
| TestValidate_ExclusiveSessionWithoutLeaseStore | validates exclusive session without lease store | unit | config | pass |
| TestValidate_RouteNoBindings | validates route with no bindings rejection | unit | config | pass |
| TestValidate_RouteBadBindingRef | validates bad binding reference rejection | unit | config | pass |
| TestValidate_InvalidAckAfter | validates invalid ack-after rejection | unit | config | pass |
| TestValidate_DirectHold | validates direct-hold config validation | unit | config | pass |
| TestValidate_ClusteredMQTTWithoutSharePrefix_Error | validates clustered MQTT bare topic rejected | unit | config | pass |
| TestValidate_ClusteredMQTTWithSharePrefix_OK | validates clustered MQTT $share/ topic accepted | unit | config | pass |
| TestValidate_ClusteredMQTTExclusiveSession_OK | validates exclusive session bypasses $share/ check | unit | config | pass |
| TestValidate_StandaloneMode_NoCheck | validates standalone mode skips shared sub check | unit | config | pass |
| TestValidate_ClusteredNonMQTT_NoCheck | validates non-MQTT receiver skips shared sub check | unit | config | pass |
| TestValidate_SharedTopicMalformed_Error | validates malformed $share/ topic rejection | unit | config | pass |
| TestValidate_MixedTopics_OneBareTopic_Error | validates mixed $share/ and bare topics error | unit | config | pass |
| TestValidate_ClusteredMQTTTransportFromSession_Error | validates transport inherited from session | unit | config | pass |
| TestValidate_EmptyDeploymentMode_NoCheck | validates empty deployment mode skips check | unit | config | pass |
| TestValidate_ClusteredMQTTMultiLevelSharedTopic_OK | validates multi-level $share/ topic accepted | unit | config | pass |
| TestValidate_ClusteredMQTTExplicitTransportNoSession_Error | validates explicit mqtt transport without session | unit | config | pass |
| TestValidate_ClusteredMQTTEmptyTopics_OK | validates empty topics pass in clustered mode | unit | config | pass |
| TestValidate_ClusteredMultiReceiver_OnlyMQTTChecked | validates only MQTT receiver checked in multi-receiver | unit | config | pass |
| TestValidate_ClusteredMQTTEphemeralSession_Error | validates ephemeral session requires $share/ topics | unit | config | pass |
| TestValidate_ClusteredMQTTShareExact_Malformed | validates $share/ alone is malformed | unit | config | pass |
| TestValidate_ClusteredMQTTCaseInsensitiveTransport_Error | validates case-insensitive MQTT transport match | unit | config | pass |
| TestValidate_ValidDirectHold | validates valid direct-hold config | unit | validate | pass |
| TestValidate_ValidSharedOutbox | validates valid shared outbox config | unit | validate | pass |
| TestValidate_DirectHold_NoVisibilityExtension | validates direct-hold without visibility extension | unit | validate | pass |
| TestValidate_DirectHold_FanOutEnabled | validates direct-hold with fan-out rejection | unit | validate | pass |
| TestValidate_DirectHold_ExclusiveSession | validates direct-hold with exclusive session | unit | validate | pass |
| TestValidate_SharedOutbox_NoOutboxStore | validates shared outbox without store | unit | validate | pass |
| TestValidate_SharedOutbox_NoLeaseStoreForExclusive | validates shared outbox without lease store for exclusive | unit | validate | pass |
| TestValidate_SharedOutbox_NoIdempotencyKey | validates shared outbox without idempotency key | unit | validate | pass |
| TestValidate_SharedOutbox_FanOutExceedsTransactionLimit | validates fan-out exceeding transaction limit | unit | validate | pass |
| TestValidate_SharedOutbox_FanOutAtLimit_OK | validates fan-out at transaction limit | unit | validate | pass |
| TestValidate_MQTT_QoS0_SharedOutbox | validates MQTT QoS0 with shared outbox | unit | validate | pass |
| TestValidate_MQTT_QoS0_DirectHold | validates MQTT QoS0 with direct hold | unit | validate | pass |
| TestValidate_MQTT_QoS2_OK | validates MQTT QoS2 acceptance | unit | validate | pass |
| TestValidate_NonMQTT_QoS0_OK | validates non-MQTT QoS0 acceptance | unit | validate | pass |
| TestValidate_EmptyRouteID | validates empty route ID rejection | unit | validate | pass |
| TestValidate_NoBindings | validates no bindings rejection | unit | validate | pass |
| TestValidate_UnknownSession | validates unknown session rejection | unit | validate | pass |
| TestValidate_UnknownDeliveryMode | validates unknown delivery mode rejection | unit | validate | pass |
| TestValidate_UnknownDispatchMode | validates unknown dispatch mode rejection | unit | validate | pass |
| TestValidate_EmptyDeliveryMode | validates empty delivery mode rejection | unit | validate | pass |
| TestValidate_BindingEmptySessionID_OK | validates binding with empty session ID | unit | validate | pass |
| TestValidate_MultipleErrors | validates multiple validation errors collected | unit | validate | pass |
| TestValidate_MultipleRoutes_IndependentErrors | validates per-route independent error collection | unit | validate | pass |
| TestValidate_SharedOutbox_IdempotencyProc_OK | validates shared outbox with idempotency processor | unit | validate | pass |
| TestValidate_NoRoutes_OK | validates empty routes acceptance | unit | validate | pass |
| TestValidate_DirectHold_MQTT_QoS0_NotReliable_OK | validates direct-hold MQTT QoS0 not reliable | unit | validate | pass |
| TestValidate_SharedOutbox_EmptyBindingSessionID | validates shared outbox with empty binding session | unit | validate | pass |
| TestValidate_SharedOutbox_CustomTransactionLimit | validates custom transaction limit | unit | validate | pass |
| TestValidate_MQTT_CaseInsensitiveTransport | validates MQTT transport case insensitive | unit | validate | pass |
| TestValidate_EmptyID_CollectsMultipleStructuralErrors | validates multiple structural errors collected | unit | validate | pass |
| TestValidate_DefaultTransactionLimit | validates default transaction limit | unit | validate | pass |
| TestBuilder_Build | validates bridge builder happy path | unit | bridge | pass |
| TestBuilder_MissingTransportFactory | validates missing transport factory rejection | unit | bridge | pass |
| TestBuilder_MissingStoreFactory | validates missing store factory rejection | unit | bridge | pass |
| TestBuilder_InvalidConfig | validates invalid config rejection | unit | bridge | pass |
| TestBuilder_DirectHoldRoute | validates direct-hold route building | unit | bridge | pass |
| TestBuilder_WithCredentialStore | validates credential store wiring | unit | bridge | pass |
| TestBuilder_CredentialInlineOverride | validates inline credential override | unit | bridge | pass |
| TestBuilder_CredentialsURIWithoutStore | validates credentials URI without store error | unit | bridge | pass |
| TestRuntime_StartStop | validates runtime start and stop lifecycle | unit | runtime | pass |
| TestRuntime_DuplicateRoute | validates duplicate route rejection | unit | runtime | pass |
| TestRuntime_AddRouteWhileRunning | validates adding route while running rejection | unit | runtime | pass |
| TestRuntime_DirectHoldEndToEnd | validates direct-hold end-to-end flow | unit | runtime | pass |
| TestRuntime_Inject_HappyPath | validates message injection happy path | unit | runtime | pass |
| TestRuntime_Inject_UnknownRoute | validates injection to unknown route rejection | unit | runtime | pass |
| TestRuntime_Inject_NotRunning | validates injection when not running rejection | unit | runtime | pass |
| TestRuntime_Inject_AssignsIDWhenEmpty | validates injection assigns ID when empty | unit | runtime | pass |
| TestRuntime_Inject_DoesNotMutateOriginal | validates injection does not mutate original | unit | runtime | pass |
| TestRuntime_SharedOutboxEndToEnd | validates shared outbox end-to-end flow | unit | runtime | pass |
| TestRunChain_Empty | validates empty processor chain | unit | runtime | pass |
| TestRunChain_Single | validates single processor chain | unit | runtime | pass |
| TestRunChain_Order | validates processor chain order | unit | runtime | pass |
| TestRunChain_Mutation | validates processor chain mutation | unit | runtime | pass |
| TestRunChain_Error | validates processor chain error propagation | unit | runtime | pass |
| TestRunChain_ShortCircuit | validates processor chain short-circuit | unit | runtime | pass |
| TestRouteRunner_DirectHold_HappyPath | validates route runner direct-hold happy path | unit | runtime | pass |
| TestRouteRunner_DirectHold_TransientSendError | validates route runner transient send error handling | unit | runtime | pass |
| TestRouteRunner_DirectHold_PermanentSendError | validates route runner permanent send error handling | unit | runtime | pass |
| TestRouteRunner_ExpiredMessage | validates route runner expired message handling | unit | runtime | pass |
| TestRouteRunner_HeaderInjection | validates route runner header injection | unit | runtime | pass |
| TestRouteRunner_ProcessorError_Permanent | validates route runner permanent processor error | unit | runtime | pass |
| TestRouteRunner_ProcessorError_Transient | validates route runner transient processor error | unit | runtime | pass |
| TestRouteRunner_ProcessorError_MessageFiltered | validates route runner filtered message handling | unit | runtime | pass |
| TestRouteRunner_Tracer_SpanLifecycle | validates route runner tracer span lifecycle | unit | runtime | pass |
| TestRouteRunner_Tracer_TraceContextExtraction | validates route runner trace context extraction | unit | runtime | pass |
| TestRouteRunner_Tracer_ContextEnrichment | validates route runner context enrichment | unit | runtime | pass |
| TestRouteRunner_Tracer_ErrorRecording | validates route runner tracer error recording | unit | runtime | pass |
| TestRouteRunner_Tracer_ProcessorErrorRecording | validates route runner processor error recording | unit | runtime | pass |
| TestRouteRunner_Tracer_FilteredNoError | validates route runner filtered message no error | unit | runtime | pass |
| TestRouteRunner_SharedOutbox_HappyPath | validates route runner shared outbox happy path | unit | runtime | pass |
| TestRouteRunner_SharedOutbox_DuplicatePersist | validates route runner duplicate persist handling | unit | runtime | pass |
| TestRouteRunner_DirectHold_WithResolver | validates route runner direct-hold with resolver | unit | runtime | pass |
| TestRouteRunner_DirectHold_ResolverError_Rejected | validates route runner resolver rejected error | unit | runtime | pass |
| TestRouteRunner_DirectHold_ResolverError_Transient | validates route runner resolver transient error | unit | runtime | pass |
| TestRouteRunner_DirectHold_ResolverHeaders | validates route runner resolver header merging | unit | runtime | pass |
| TestRouteRunner_SharedOutbox_FanOut | validates route runner shared outbox fan-out | unit | runtime | pass |
| TestRouteRunner_SharedOutbox_ResolverError_Rejected | validates route runner outbox resolver error | unit | runtime | pass |
| TestRouteRunner_Backpressure | validates route runner backpressure handling | unit | runtime | pass |
| TestRouteRunner_MQTTToSQS_DirectHold | validates route runner MQTT to SQS direct-hold | unit | runtime | pass |
| TestRouteRunner_MQTTToSQS_SharedOutbox | validates route runner MQTT to SQS shared outbox | unit | runtime | pass |
| TestRouteRunner_EmitsE2ELatency | validates route runner emits e2e latency metric | integration | runtime | pass |
| TestRouteRunner_EmitsDLQEntries | validates route runner emits DLQ entry metric | integration | runtime | pass |
| TestOutboxDrainer_EmitsDrainLatency | validates outbox drainer emits drain latency metric | integration | runtime | pass |
| TestOutboxDrainer_EmitsExpiredBeforeSend | validates outbox drainer emits expired-before-send metric | integration | runtime | pass |
| TestSessionManager_EmitsLeaseMetrics | validates session manager emits lease metrics | integration | runtime | pass |
| TestSessionManager_EmitsReconnectMetric | validates session manager emits reconnect metric | integration | runtime | pass |
| TestMetrics_FullPipeline_DirectHold | validates full pipeline metrics for direct-hold | unit | runtime | pass |
| TestMetrics_FullPipeline_SharedOutbox | validates full pipeline metrics for shared outbox | unit | runtime | pass |
| TestMetrics_AllMetricNamesDocumented | validates all metric names are documented | unit | runtime | pass |
| TestSessionManager_NonExclusive | validates session manager non-exclusive mode | unit | runtime | pass |
| TestSessionManager_ExclusiveLease | validates session manager exclusive lease mode | unit | runtime | pass |
| TestSessionManager_StepDown | validates session manager step-down behavior | unit | runtime | pass |
| TestSessionManager_Close | validates session manager close behavior | unit | runtime | pass |
| TestCredentialResolver_SingleSchemeDispatch | validates credential resolver single scheme | unit | runtime | pass |
| TestCredentialResolver_MultiSchemeDispatch | validates credential resolver multi scheme | unit | runtime | pass |
| TestCredentialResolver_NamespaceLongestPrefix | validates credential resolver namespace prefix matching | unit | runtime | pass |
| TestCredentialResolver_NotFoundError | validates credential resolver not-found error | unit | runtime | pass |
| TestCredentialResolver_CacheHitMiss | validates credential resolver cache hit/miss | unit | runtime | pass |
| TestCredentialResolver_CacheExpiry | validates credential resolver cache expiry | unit | runtime | pass |
| TestCredentialResolver_CacheDisabled | validates credential resolver cache disabled | unit | runtime | pass |
| TestCredentialResolver_InvalidateCache | validates credential resolver cache invalidation | unit | runtime | pass |
| TestCredentialResolver_ClearCache | validates credential resolver cache clearing | unit | runtime | pass |
| TestSharedOutbox_BasicFlow | validates shared outbox basic flow | integration | runtime | pass |
| TestSharedOutbox_ProcessorChainRuns | validates shared outbox processor chain execution | integration | runtime | pass |
| TestSharedOutbox_CorrelationIDInjected | validates shared outbox correlation ID injection | integration | runtime | pass |
| TestSharedOutbox_ReservedHeadersStripped | validates shared outbox reserved header stripping | integration | runtime | pass |
| TestRenderAddress_HappyPath | validates address template rendering | unit | runtime | pass |
| TestRenderAddress_MultiplePlaceholders | validates multiple placeholder rendering | unit | runtime | pass |
| TestRenderAddress_NoPlaceholders | validates no placeholder passthrough | unit | runtime | pass |
| TestRenderAddress_EmptyTemplate | validates empty template handling | unit | runtime | pass |
| TestRenderAddress_MissingPlaceholder | validates missing placeholder error | unit | runtime | pass |
| TestRenderAddress_EmptyPlaceholderKey | validates empty placeholder key error | unit | runtime | pass |
| TestRenderAddress_RendersToEmpty | validates rendering to empty string | unit | runtime | pass |
| TestValidateMQTTTopic_ValidTopics | validates valid MQTT topic acceptance | unit | runtime | pass |
| TestValidateMQTTTopic_Empty | validates empty MQTT topic rejection | unit | runtime | pass |
| TestValidateMQTTTopic_PlusWildcard | validates plus wildcard rejection | unit | runtime | pass |
| TestValidateMQTTTopic_HashWildcard | validates hash wildcard rejection | unit | runtime | pass |
| TestValidateMQTTTopic_NullCharacter | validates null character rejection | unit | runtime | pass |
| TestValidateMQTTTopic_EmptySegment | validates empty segment rejection | unit | runtime | pass |
| TestValidateMQTTTopic_LeadingSlash | validates leading slash handling | unit | runtime | pass |
| TestValidateMQTTTopic_TrailingSlash | validates trailing slash handling | unit | runtime | pass |
| TestBindingResolver_MatchByHeader_SingleMatch | validates binding resolver single header match | unit | runtime | pass |
| TestBindingResolver_MatchByHeader_NoMatch | validates binding resolver no header match | unit | runtime | pass |
| TestBindingResolver_MatchByHeader_MissingHeader | validates binding resolver missing header | unit | runtime | pass |
| TestBindingResolver_MatchAll_FanOut | validates binding resolver match-all fan-out | unit | runtime | pass |
| TestBindingResolver_MatchByID | validates binding resolver match by ID | unit | runtime | pass |
| TestBindingResolver_MatchByID_NotFound | validates binding resolver ID not found | unit | runtime | pass |
| TestBindingResolver_MQTTTopicValidation | validates binding resolver MQTT topic validation | unit | runtime | pass |
| TestBindingResolver_NonMQTTSkipsTopicValidation | validates non-MQTT skips topic validation | unit | runtime | pass |
| TestBindingResolver_AddressTemplateError | validates binding resolver address template error | unit | runtime | pass |
| TestBindingResolver_OptionsAsDispatchHeaders | validates binding options as dispatch headers | unit | runtime | pass |
| TestBindingResolver_OptionsNotShared | validates binding options not shared between calls | unit | runtime | pass |
| TestStaticResolver_ReturnsSamePlans | validates static resolver returns same plans | unit | runtime | pass |
| TestStaticResolver_IndependentOfEnvelope | validates static resolver independent of envelope | unit | runtime | pass |
| TestOutboxDrainer_HappyPath | validates outbox drainer happy path | unit | runtime | pass |
| TestOutboxDrainer_ExpiredRecord | validates outbox drainer expired record handling | unit | runtime | pass |
| TestOutboxDrainer_PoisonMessage | validates outbox drainer poison message handling | unit | runtime | pass |
| TestOutboxDrainer_StaleFencingToken | validates outbox drainer stale fencing token | unit | runtime | pass |
| TestOutboxDrainer_NoLease | validates outbox drainer no lease behavior | unit | runtime | pass |
| TestOutboxDrainer_AppliesAddress | validates outbox drainer applies address | unit | runtime | pass |
| TestOutboxDrainer_EmptyAddressPreservesSubject | validates outbox drainer empty address preserves subject | unit | runtime | pass |
| TestOutboxDrainer_PermanentSendError | validates outbox drainer permanent send error | unit | runtime | pass |
| TestFanOut_SingleRouteMultipleSessions | validates fan-out single route multiple sessions | unit | runtime | pass |
| TestFanOut_PartialSessionAvailability | validates fan-out partial session availability | unit | runtime | pass |
| TestFanOut_RegisterSessionSenderWhileRunning | validates fan-out register sender while running | unit | runtime | pass |
| TestEdge_StaleFencingTokenRejected | validates stale fencing token rejection | unit | runtime | pass |
| TestEdge_IdempotentPersist | validates idempotent persist behavior | unit | runtime | pass |
| TestEdge_ExpiredOutboxEntry | validates expired outbox entry handling | unit | runtime | pass |
| TestEdge_ExpiredOutboxEntryDuringDrain | validates expired entry during drain | unit | runtime | pass |
| TestEdge_PoisonMessageDLQ | validates poison message DLQ routing | unit | runtime | pass |
| TestEdge_CrashBeforeOutboxPersist | validates crash before outbox persist | unit | runtime | pass |
| TestEdge_CrashAfterPersistBeforeAck | validates crash after persist before ack | unit | runtime | pass |
| TestEdge_CrashAfterAckBeforeSend | validates crash after ack before send | unit | runtime | pass |
| TestEdge_PermanentSendErrorGoesToDLQ | validates permanent send error DLQ routing | unit | runtime | pass |
| TestEdge_CrashAfterSendBeforeCompletion | validates crash after send before completion | unit | runtime | pass |
| TestEdge_FanOutPartialPersist | validates fan-out partial persist handling | unit | runtime | pass |
| TestDLQRouter_NilStore | validates DLQ router nil store behavior | unit | runtime | pass |
| TestDLQRouter_WritesEntry | validates DLQ router writes entry | unit | runtime | pass |
| TestDLQRouter_UnknownError | validates DLQ router unknown error handling | unit | runtime | pass |
| TestValidator_DirectHold_Valid | validates direct-hold valid config | unit | runtime | pass |
| TestValidator_DirectHold_RejectsFanOut | validates direct-hold rejects fan-out | unit | runtime | pass |
| TestValidator_DirectHold_RejectsExclusiveSession | validates direct-hold rejects exclusive session | unit | runtime | pass |
| TestValidator_DirectHold_RejectsMissingVisibilityExtension | validates direct-hold rejects missing visibility ext | unit | runtime | pass |
| TestValidator_DirectHold_RejectsMultipleBindings | validates direct-hold rejects multiple bindings | unit | runtime | pass |
| TestValidator_DirectHold_CollectsMultipleErrors | validates direct-hold collects multiple errors | unit | runtime | pass |
| TestValidator_DirectHold_NoSession | validates direct-hold no session | unit | runtime | pass |
| TestValidator_SharedOutbox_Valid | validates shared outbox valid config | unit | runtime | pass |
| TestValidator_SharedOutbox_RejectsMissingOutboxStore | validates shared outbox rejects missing store | unit | runtime | pass |
| TestValidator_DirectHold_DefaultDeliveryMode | validates direct-hold default delivery mode | unit | runtime | pass |
| TestValidator_MultipleRouteErrors | validates multiple route errors collected | unit | runtime | pass |
| TestValidator_SharedOutbox_NonExclusiveNoLeaseStore | validates non-exclusive no lease store | unit | runtime | pass |
| TestValidator_SharedOutbox_RejectsMissingLeaseStoreForExclusive | validates exclusive missing lease store rejection | unit | runtime | pass |
| TestInstrumentedLeaseStore_AcquireRecordsLatency | validates instrumented lease store acquire latency | unit | runtime | pass |
| TestInstrumentedLeaseStore_AcquireFailureRecordsCounter | validates instrumented lease store acquire failure | unit | runtime | pass |
| TestInstrumentedLeaseStore_RenewRecordsLatency | validates instrumented lease store renew latency | unit | runtime | pass |
| TestInstrumentedOutboxStore_PersistRecordsLatency | validates instrumented outbox store persist latency | unit | runtime | pass |
| TestInstrumentedOutboxStore_CompleteDelegates | validates instrumented outbox store complete delegation | unit | runtime | pass |
| TestInstrumentedOutboxStore_QueryPendingRecordsDepth | validates instrumented outbox store query depth | unit | runtime | pass |
| TestInstrumentedSender_RecordsSendLatency | validates instrumented sender send latency | unit | runtime | pass |
| TestInstrumentedReceiver_RecordsReceiveLatency | validates instrumented receiver receive latency | unit | runtime | pass |
| TestInstrumentedDelivery_ExtendCountsVisibilityExtension | validates instrumented delivery extend counter | unit | runtime | pass |
| TestCrossInstance_SQSConsumerAndMQTTOwnerAreDifferent | validates cross-instance SQS consumer and MQTT owner | unit | runtime | pass |
| TestCrossInstance_LeaseTransferDrainsRemaining | validates cross-instance lease transfer drains remaining | unit | runtime | pass |
| TestCrossInstance_ConnectAfterLease | validates cross-instance connect after lease | unit | runtime | pass |
| TestCrossInstance_MultipleMessages | validates cross-instance multiple messages | unit | runtime | pass |
| TestValidateConfig_AdminAPIKeyRequired | validates admin API key required | unit | httpapi | pass |
| TestValidateConfig_CORSWildcardRejected | validates CORS wildcard rejection | unit | httpapi | pass |
| TestValidateConfig_CORSWildcardInListRejected | validates CORS wildcard in list rejection | unit | httpapi | pass |
| TestValidateConfig_ExplicitCORSAllowed | validates explicit CORS origin allowed | unit | httpapi | pass |
| TestValidateConfig_EmptyCORSAllowed | validates empty CORS allowed | unit | httpapi | pass |
| TestAdminAuth_RequiredWhenKeySet | validates admin auth required when key set | unit | httpapi | pass |
| TestMonitorProbes_NoAuthRequired | validates monitor probes no auth required | unit | httpapi | pass |
| TestMonitorSensitive_RequiresAuth | validates monitor sensitive requires auth | unit | httpapi | pass |
| TestMonitorSensitive_SeparateMonitorKey | validates separate monitor key | unit | httpapi | pass |
| TestCORS_DisabledByDefault | validates CORS disabled by default | unit | httpapi | pass |
| TestCORS_ExplicitOriginAllowed | validates CORS explicit origin allowed | unit | httpapi | pass |
| TestCORS_UnlistedOriginRejected | validates CORS unlisted origin rejected | unit | httpapi | pass |
| TestCORS_PreflightReturns204 | validates CORS preflight returns 204 | unit | httpapi | pass |
| TestHandleBridge | validates handle bridge endpoint | unit | httpapi | pass |
| TestHandleRoutes_Empty | validates handle routes with empty routes | unit | httpapi | pass |
| TestHandleHealth | validates handle health endpoint | unit | httpapi | pass |
| TestHandleLive | validates handle live endpoint | unit | httpapi | pass |
| TestHandleReady_NotRunning | validates handle ready not running | unit | httpapi | pass |
| TestMethodNotAllowed | validates method not allowed response | unit | httpapi | pass |
| TestDLQ_NoStore | validates DLQ endpoint without store | unit | httpapi | pass |
| TestAuditLogging_AdminCalls | validates audit logging for admin calls | unit | httpapi | pass |
| TestAuditLogging_DLQPurge | validates audit logging for DLQ purge | unit | httpapi | pass |
| TestAuditLogging_DLQReplay | validates audit logging for DLQ replay | unit | httpapi | pass |
| TestInject_RequiresAuth | validates inject requires auth | unit | httpapi | pass |
| TestInject_UnknownRoute | validates inject unknown route rejection | unit | httpapi | pass |
| TestInject_InvalidBody | validates inject invalid body rejection | unit | httpapi | pass |
| TestInject_InvalidBase64 | validates inject invalid base64 rejection | unit | httpapi | pass |
| TestInject_HappyPath | validates inject happy path | unit | httpapi | pass |
| TestHandleTopology | validates handle topology endpoint | unit | httpapi | pass |
| TestSlogAuditLogger_LogsEvent | validates slog audit logger logs event | unit | httpapi | pass |
| TestCorrelationMW_ExtractsExistingCorrelationID | validates correlation MW extracts existing ID | unit | httpapi | pass |
| TestCorrelationMW_FallsBackToXRequestID | validates correlation MW X-Request-ID fallback | unit | httpapi | pass |
| TestCorrelationMW_PrefersCorrelationIDOverRequestID | validates correlation MW prefers correlation ID | unit | httpapi | pass |
| TestCorrelationMW_GeneratesCorrelationIDWhenMissing | validates correlation MW generates ID when missing | unit | httpapi | pass |
| TestCorrelationMW_ParsesValidTraceparent | validates correlation MW parses valid traceparent | unit | httpapi | pass |
| TestCorrelationMW_InvalidTraceparentIgnored | validates correlation MW ignores invalid traceparent | unit | httpapi | pass |
| TestCorrelationMW_FallsBackToXTraceIDAndXSpanID | validates correlation MW legacy header fallback | unit | httpapi | pass |
| TestCorrelationMW_TraceparentOverridesLegacyHeaders | validates traceparent overrides legacy headers | unit | httpapi | pass |
| TestCorrelationMW_GeneratesTraceAndSpanIDsWhenMissing | validates MW generates trace/span IDs | unit | httpapi | pass |
| TestCorrelationMW_AllIDsInContext | validates MW all IDs in context | unit | httpapi | pass |
| TestCorrelationMW_ResponseHeadersPresent | validates MW response headers present | unit | httpapi | pass |
| TestCorrelationMW_GeneratedIDsAreUnique | validates MW generated IDs are unique | unit | httpapi | pass |
| TestCorrelationMW_OnlySpanIDFromLegacy | validates MW only span ID from legacy | unit | httpapi | pass |
| TestCorrelationMW_IntegrationWithWrap | validates MW integration with wrap handler | unit | httpapi | pass |
| TestCorrelationIDRoundTrip | validates correlation ID round trip | unit | observability | pass |
| TestTraceIDRoundTrip | validates trace ID round trip | unit | observability | pass |
| TestSpanIDRoundTrip | validates span ID round trip | unit | observability | pass |
| TestMissingValuesReturnEmpty | validates missing values return empty | unit | observability | pass |
| TestContextLayering | validates context layering | unit | observability | pass |
| TestOverwrite | validates context value overwrite | unit | observability | pass |
| TestCorrelationHandler_AllFields | validates correlation handler all fields | unit | observability | pass |
| TestCorrelationHandler_PartialFields | validates correlation handler partial fields | unit | observability | pass |
| TestCorrelationHandler_NoFields | validates correlation handler no fields | unit | observability | pass |
| TestCorrelationHandler_WithAttrs | validates correlation handler with attrs | unit | observability | pass |
| TestCorrelationHandler_WithGroup | validates correlation handler with group | unit | observability | pass |
| TestCorrelationHandler_Enabled | validates correlation handler enabled | unit | observability | pass |
| TestStateTransitions_ClosedToOpenToHalfOpenToClosed | validates circuit breaker state transitions | unit | circuitbreaker | pass |
| TestHalfOpen_FailureReopens | validates half-open failure reopens circuit | unit | circuitbreaker | pass |
| TestPerKeyIsolation | validates per-key circuit isolation | unit | circuitbreaker | pass |
| TestRetryAfterPropagation | validates retry-after propagation | unit | circuitbreaker | pass |
| TestConcurrentSafety | validates concurrent safety | unit | circuitbreaker | pass |
| TestKeyExtractors | validates key extractors | unit | circuitbreaker | pass |
| TestConfigDefaults | validates config defaults | unit | circuitbreaker | pass |
| TestProcessorName | validates processor name | unit | circuitbreaker | pass |
| TestMetrics | validates circuit breaker metrics | unit | circuitbreaker | pass |
| TestOnStateChangeCallback | validates on-state-change callback | unit | circuitbreaker | pass |
| TestNextErrorPropagation | validates next error propagation | unit | circuitbreaker | pass |
| TestJSONTransform_SimpleMapping | validates JSON transform simple mapping | unit | transform | pass |
| TestJSONTransform_NestedExtraction | validates JSON transform nested extraction | unit | transform | pass |
| TestJSONTransform_NestedTarget | validates JSON transform nested target | unit | transform | pass |
| TestJSONTransform_TypeTransformation | validates JSON transform type transformation | unit | transform | pass |
| TestJSONTransform_DefaultValue | validates JSON transform default value | unit | transform | pass |
| TestJSONTransform_RequiredField | validates JSON transform required field | unit | transform | pass |
| TestJSONTransform_Base64Encoding | validates JSON transform base64 encoding | unit | transform | pass |
| TestJSONTransform_ArrayAccess | validates JSON transform array access | unit | transform | pass |
| TestJSONTransform_EmptyPayload | validates JSON transform empty payload | unit | transform | pass |
| TestJSONTransform_InvalidJSONPath | validates JSON transform invalid JSON path | unit | transform | pass |
| TestDropFilter_MatchingMessages | validates drop filter matching messages | unit | filter | pass |
| TestPassFilter_MatchingMessages | validates pass filter matching messages | unit | filter | pass |
| TestRouteFilter_SetsHeaderAndCallsNext | validates route filter sets header and calls next | unit | filter | pass |
| TestCondition_AllOperators | validates condition all operators | unit | filter | pass |
| TestCondition_JSONPath | validates condition JSON path | unit | filter | pass |
| TestCondition_SubjectField | validates condition subject field | unit | filter | pass |
| TestCondition_HeaderField | validates condition header field | unit | filter | pass |
| TestCondition_BareFieldFallback | validates condition bare field fallback | unit | filter | pass |
| TestFilter_MultipleConditions | validates filter multiple conditions | unit | filter | pass |
| TestFilter_Inversion | validates filter inversion | unit | filter | pass |
| TestFilter_NextErrorPropagation | validates filter next error propagation | unit | filter | pass |
| TestFilter_InvalidRegex | validates filter invalid regex | unit | filter | pass |
| TestFilter_RouteRequiresRouteTo | validates filter route requires route-to | unit | filter | pass |
| TestProcessor_Name | validates filter processor name | unit | filter | pass |
| TestFilter_NoConditionsAlwaysMatches | validates filter no conditions always matches | unit | filter | pass |
| TestFilter_NilHeaders | validates filter nil headers | unit | filter | pass |
| TestFilter_EmptyPayload | validates filter empty payload | unit | filter | pass |
| TestProcess_NoTenantHeader_NotRequired | validates no tenant header not required | unit | tenant | pass |
| TestProcess_NoTenantHeader_Required | validates no tenant header required rejection | unit | tenant | pass |
| TestProcess_ValidTenant_PassesThrough | validates valid tenant passes through | unit | tenant | pass |
| TestProcess_InactiveTenant_Rejected | validates inactive tenant rejection | unit | tenant | pass |
| TestProcess_ValidationError_Propagated | validates validation error propagation | unit | tenant | pass |
| TestProcess_MessageSizeExceedsQuota | validates message size exceeds quota | unit | tenant | pass |
| TestProcess_MessageSizeWithinQuota | validates message size within quota | unit | tenant | pass |
| TestProcess_ZeroQuota_NoSizeCheck | validates zero quota no size check | unit | tenant | pass |
| TestProcess_UsageTracker_InFlightAndMessages | validates usage tracker in-flight and messages | unit | tenant | pass |
| TestProcess_UsageTracker_NoMessageCountOnError | validates no message count on error | unit | tenant | pass |
| TestProcess_InFlightTrackingError_ReturnedBeforeNext | validates in-flight tracking error | unit | tenant | pass |
| TestProcess_CustomTenantHeader | validates custom tenant header | unit | tenant | pass |
| TestProcess_NextErrorPropagation | validates next error propagation | unit | tenant | pass |
| TestProcessor_Name | validates tenant processor name | unit | tenant | pass |
| TestProcess_NoValidator_SkipsValidation | validates no validator skips validation | unit | tenant | pass |
| TestProcess_NilHeaders_NotRequired | validates nil headers not required | unit | tenant | pass |
| TestProcess_ValidatorAndTracker_FullFlow | validates validator and tracker full flow | unit | tenant | pass |
| TestBridgeFactory_NewSession_ReturnsNilNil | validates SQS bridge factory session returns nil | unit | sqs | pass |
| TestBridgeFactory_Capabilities | validates SQS bridge factory capabilities | unit | sqs | pass |
| TestBridgeFactory_NewReceiver | validates SQS bridge factory new receiver | unit | sqs | pass |
| TestBridgeFactory_NewSender | validates SQS bridge factory new sender | unit | sqs | pass |
| TestBridgeFactory_NewReceiver_OptionsPassthrough | validates SQS receiver options passthrough | unit | sqs | pass |
| TestBridgeFactory_NewSender_OptionsPassthrough | validates SQS sender options passthrough | unit | sqs | pass |
| TestDelivery_Envelope | validates SQS delivery envelope | unit | sqs | pass |
| TestDelivery_Ack_DeletesMessage | validates SQS delivery ack deletes message | unit | sqs | pass |
| TestDelivery_Ack_Error | validates SQS delivery ack error | unit | sqs | pass |
| TestDelivery_Retry_ZeroDelay | validates SQS delivery retry zero delay | unit | sqs | pass |
| TestDelivery_Retry_WithDelay | validates SQS delivery retry with delay | unit | sqs | pass |
| TestDelivery_Extend | validates SQS delivery extend | unit | sqs | pass |
| TestDelivery_Extend_ClampsMax | validates SQS delivery extend clamps max | unit | sqs | pass |
| TestDelivery_AutoExtend_CallsChangeVisibility | validates SQS auto-extend calls change visibility | unit | sqs | pass |
| TestDelivery_AutoExtend_StopsOnAck | validates SQS auto-extend stops on ack | unit | sqs | pass |
| TestDelivery_AutoExtend_StopsOnRetry | validates SQS auto-extend stops on retry | unit | sqs | pass |
| TestDelivery_NoAutoExtend | validates SQS no auto-extend | unit | sqs | pass |
| TestDelivery_AutoExtend_StopsOnError | validates SQS auto-extend stops on error | unit | sqs | pass |
| TestDelivery_AutoExtend_UsesCorrectTimeout | validates SQS auto-extend correct timeout | unit | sqs | pass |
| TestDelivery_MultipleStopsAreSafe | validates SQS multiple stops are safe | unit | sqs | pass |
| TestNewDelivery_WithQueueURLAndHandle | validates SQS new delivery with queue URL | unit | sqs | pass |
| TestDelivery_Ack_StopsAutoExtendThenDeletes | validates SQS ack stops auto-extend then deletes | unit | sqs | pass |
| TestMapError_Nil | validates SQS map error nil | unit | sqs | pass |
| TestMapError_ContextDeadline | validates SQS map error context deadline | unit | sqs | pass |
| TestMapError_ContextCanceled | validates SQS map error context canceled | unit | sqs | pass |
| TestMapError_QueueDoesNotExist | validates SQS map error queue not exist | unit | sqs | pass |
| TestMapError_MessageNotInflight | validates SQS map error message not inflight | unit | sqs | pass |
| TestMapError_ReceiptHandleIsInvalid | validates SQS map error invalid receipt handle | unit | sqs | pass |
| TestMapError_OverLimit | validates SQS map error over limit | unit | sqs | pass |
| TestMapError_BatchRequestTooLong | validates SQS map error batch too long | unit | sqs | pass |
| TestMapError_UnsupportedOperation | validates SQS map error unsupported operation | unit | sqs | pass |
| TestMapError_StringPatterns | validates SQS map error string patterns | unit | sqs | pass |
| TestMapError_IsRecoverable | validates SQS map error is recoverable | unit | sqs | pass |
| TestAttributesToHeaders_StripsReservedPrefix | validates SQS attributes strips reserved prefix | unit | sqs | pass |
| TestAttributesToHeaders_BinaryValue | validates SQS attributes binary value | unit | sqs | pass |
| TestAttributesToHeaders_SystemAttributes | validates SQS attributes system attributes | unit | sqs | pass |
| TestHeadersToAttributes_Basic | validates SQS headers to attributes basic | unit | sqs | pass |
| TestHeadersToAttributes_ExcludesFIFOFields | validates SQS headers excludes FIFO fields | unit | sqs | pass |
| TestHeadersToAttributes_Nil | validates SQS headers to attributes nil | unit | sqs | pass |
| TestHeadersToAttributes_EmptyValues | validates SQS headers to attributes empty values | unit | sqs | pass |
| TestExtractFIFOFields | validates SQS extract FIFO fields | unit | sqs | pass |
| TestExtractFIFOFields_Missing | validates SQS extract FIFO fields missing | unit | sqs | pass |
| TestExtractFIFOFields_Nil | validates SQS extract FIFO fields nil | unit | sqs | pass |
| TestReceiver_RunEmitsDeliveries | validates SQS receiver run emits deliveries | unit | sqs | pass |
| TestReceiver_RunStripsReservedHeaders | validates SQS receiver strips reserved headers | unit | sqs | pass |
| TestReceiver_RunSNSUnwrap | validates SQS receiver SNS unwrap | unit | sqs | pass |
| TestReceiver_RunRetriesOnReceiveError | validates SQS receiver retries on error | unit | sqs | pass |
| TestReceiver_RunStopsOnEmitError | validates SQS receiver stops on emit error | unit | sqs | pass |
| TestReceiver_Validate_RequiresQueue | validates SQS receiver requires queue | unit | sqs | pass |
| TestReceiver_DeliveryAck_DeletesCorrectHandle | validates SQS delivery ack deletes correct handle | unit | sqs | pass |
| TestSender_Send_Basic | validates SQS sender send basic | unit | sqs | pass |
| TestSender_Send_FIFO_WithHeaders | validates SQS sender FIFO with headers | unit | sqs | pass |
| TestSender_Send_FIFO_DefaultGroup | validates SQS sender FIFO default group | unit | sqs | pass |
| TestSender_Send_FIFO_HeaderOverridesDefault | validates SQS sender FIFO header overrides | unit | sqs | pass |
| TestSender_Send_WithDelay | validates SQS sender send with delay | unit | sqs | pass |
| TestSender_Send_Error | validates SQS sender send error | unit | sqs | pass |
| TestSender_SendBatch_Basic | validates SQS sender send batch basic | unit | sqs | pass |
| TestSender_SendBatch_PartialFailure | validates SQS sender batch partial failure | unit | sqs | pass |
| TestSender_SendBatch_LargeBatch | validates SQS sender batch large batch | unit | sqs | pass |
| TestSender_Validate_RequiresQueue | validates SQS sender requires queue | unit | sqs | pass |
| TestSender_ConfigDefaults | validates SQS sender config defaults | unit | sqs | pass |
| TestSender_ConfigDefaults_Clamps | validates SQS sender config defaults clamps | unit | sqs | pass |
| TestGenerateDeduplicationID_Deterministic | validates deduplication ID deterministic | unit | sqs | pass |
| TestGenerateDeduplicationID_DiffersOnPayload | validates deduplication ID differs on payload | unit | sqs | pass |
| TestDynamoDBStoreFactory_NewLeaseStore | validates DynamoDB store factory new lease store | unit | aws_store | pass |
| TestDynamoDBStoreFactory_NewOutboxStore | validates DynamoDB store factory new outbox store | unit | aws_store | pass |
| TestDynamoDBStoreFactory_NewDLQStore | validates DynamoDB store factory new DLQ store | unit | aws_store | pass |
| TestDynamoDBStoreFactory_WithTableName | validates DynamoDB store factory with table name | unit | aws_store | pass |
| TestMain | validates DynamoDB DLQ test setup | integration | dynamodbdlq | pass |
| TestDLQStoreConformance | validates DynamoDB DLQ store conformance | integration | dynamodbdlq | pass |
| TestWriteAndList | validates DynamoDB DLQ write and list | integration | dynamodbdlq | pass |
| TestListFilterByRouteID | validates DynamoDB DLQ list filter by route | integration | dynamodbdlq | pass |
| TestListFilterByCategory | validates DynamoDB DLQ list filter by category | integration | dynamodbdlq | pass |
| TestListFilterBySince | validates DynamoDB DLQ list filter by since | integration | dynamodbdlq | pass |
| TestListFilterByBefore | validates DynamoDB DLQ list filter by before | integration | dynamodbdlq | pass |
| TestListRespectsLimit | validates DynamoDB DLQ list respects limit | integration | dynamodbdlq | pass |
| TestWriteIdempotent | validates DynamoDB DLQ write idempotent | integration | dynamodbdlq | pass |
| TestReplayMarksEntries | validates DynamoDB DLQ replay marks entries | integration | dynamodbdlq | pass |
| TestPurgeRemovesOld | validates DynamoDB DLQ purge removes old | integration | dynamodbdlq | pass |
| TestPurgeSkipsRecent | validates DynamoDB DLQ purge skips recent | integration | dynamodbdlq | pass |
| TestFullLifecycle | validates DynamoDB DLQ full lifecycle | integration | dynamodbdlq | pass |
| TestEnsureTableIdempotent | validates DynamoDB DLQ ensure table idempotent | integration | dynamodbdlq | pass |
| TestListBothRouteAndCategory | validates DynamoDB DLQ list both filters | integration | dynamodbdlq | pass |
| TestMain | validates DynamoDB outbox test setup | integration | dynamodboutbox | pass |
| TestOutboxStoreConformance | validates DynamoDB outbox conformance | integration | dynamodboutbox | pass |
| TestIdempotentPersistAfterRedelivery | validates DynamoDB outbox idempotent persist | integration | dynamodboutbox | pass |
| TestFanOutAtomicity | validates DynamoDB outbox fan-out atomicity | integration | dynamodboutbox | pass |
| TestFanOutDuplicateRejection | validates DynamoDB outbox fan-out duplicate rejection | integration | dynamodboutbox | pass |
| TestConcurrentClaimSafety | validates DynamoDB outbox concurrent claim safety | integration | dynamodboutbox | pass |
| TestReplayCountIncrementsOnReclaim | validates DynamoDB outbox replay count increment | integration | dynamodboutbox | pass |
| TestCompleteWithStaleTokenRejected | validates DynamoDB outbox stale token rejection | integration | dynamodboutbox | pass |
| TestExpireWithNoExpirySetSkips | validates DynamoDB outbox expire with no expiry skips | integration | dynamodboutbox | pass |
| TestDispatchHeadersRoundTrip | validates DynamoDB outbox dispatch headers round trip | integration | dynamodboutbox | pass |
| TestEnvelopePayloadRoundTrip | validates DynamoDB outbox envelope payload round trip | integration | dynamodboutbox | pass |
| TestCreateTableIdempotent | validates DynamoDB outbox create table idempotent | integration | dynamodboutbox | pass |
| TestFullLifecycleWithExpiry | validates DynamoDB outbox full lifecycle with expiry | integration | dynamodboutbox | pass |
| TestMain | validates DynamoDB lease test setup | integration | dynamodblease | pass |
| TestConformanceSuite | validates DynamoDB lease conformance suite | integration | dynamodblease | pass |
| TestDynamoDBSpecificErrorMapping | validates DynamoDB specific error mapping | integration | dynamodblease | pass |
| TestMain | validates DynamoDB config loader test setup | integration | dynamodb_config | pass |
| TestLoadSuccess | validates DynamoDB config load success | integration | dynamodb_config | pass |
| TestLoadNotFound | validates DynamoDB config load not found | integration | dynamodb_config | pass |
| TestWatchDetectsChanges | validates DynamoDB config watch detects changes | integration | dynamodb_config | pass |
| TestWatchNoDuplicates | validates DynamoDB config watch no duplicates | integration | dynamodb_config | pass |
| TestSaveIncrementsVersion | validates DynamoDB config save increments version | integration | dynamodb_config | pass |
| TestEnsureTableIdempotent | validates DynamoDB config ensure table idempotent | integration | dynamodb_config | pass |
| TestParseCredentials_JSONUsernamePassword | validates SSM parse JSON username/password | unit | ssm | pass |
| TestParseCredentials_SimpleFormat | validates SSM parse simple format | unit | ssm | pass |
| TestParseCredentials_SimpleFormat_PasswordWithColon | validates SSM parse password with colon | unit | ssm | pass |
| TestParseCredentials_TLS | validates SSM parse TLS credentials | unit | ssm | pass |
| TestParseCredentials_TLS_InsecureSkipVerify | validates SSM parse TLS insecure skip verify | unit | ssm | pass |
| TestParseCredentials_TLS_SingleCAPem | validates SSM parse TLS single CA PEM | unit | ssm | pass |
| TestParseCredentials_UnsupportedFormat | validates SSM parse unsupported format | unit | ssm | pass |
| TestParseCredentials_InvalidJSON | validates SSM parse invalid JSON | unit | ssm | pass |
| TestParseCredentials_UnknownJSONType | validates SSM parse unknown JSON type | unit | ssm | pass |
| TestParseCredentials_MissingUsername | validates SSM parse missing username | unit | ssm | pass |
| TestSerializeAndParseRoundTrip_Password | validates SSM serialize/parse round trip password | unit | ssm | pass |
| TestSerializeAndParseRoundTrip_TLS | validates SSM serialize/parse round trip TLS | unit | ssm | pass |
| TestParseURI | validates SSM parse URI | unit | ssm | pass |
| TestPathToURI | validates SSM path to URI | unit | ssm | pass |
| TestRepository_Scheme | validates SSM repository scheme | unit | ssm | pass |
| TestRepository_Namespace | validates SSM repository namespace | unit | ssm | pass |
| TestRepository_Namespace_Empty | validates SSM repository empty namespace | unit | ssm | pass |
| TestRepository_Get | validates SSM repository get | unit | ssm | pass |
| TestRepository_Get_NotFound | validates SSM repository get not found | unit | ssm | pass |
| TestRepository_Get_NilValue | validates SSM repository get nil value | unit | ssm | pass |
| TestRepository_Create | validates SSM repository create | unit | ssm | pass |
| TestRepository_Create_AlreadyExists | validates SSM repository create already exists | unit | ssm | pass |
| TestRepository_Update | validates SSM repository update | unit | ssm | pass |
| TestRepository_Update_VersionMismatch | validates SSM repository update version mismatch | unit | ssm | pass |
| TestRepository_Delete | validates SSM repository delete | unit | ssm | pass |
| TestRepository_Delete_VersionCheck | validates SSM repository delete version check | unit | ssm | pass |
| TestRepository_List | validates SSM repository list | unit | ssm | pass |
| TestRepository_List_WithPrefix | validates SSM repository list with prefix | unit | ssm | pass |
| TestRepository_List_Pagination | validates SSM repository list pagination | unit | ssm | pass |
| TestMapAWSError_Nil | validates SSM map AWS error nil | unit | ssm | pass |
| TestMapAWSError_ParameterNotFound | validates SSM map AWS error not found | unit | ssm | pass |
| TestMapAWSError_ParameterAlreadyExists | validates SSM map AWS error already exists | unit | ssm | pass |
| TestMapAWSError_GenericError | validates SSM map AWS error generic | unit | ssm | pass |
| TestRepository_Get_InvalidURI | validates SSM repository get invalid URI | unit | ssm | pass |
| TestRepository_Create_InvalidURI | validates SSM repository create invalid URI | unit | ssm | pass |
| TestDefaultAlarms_Count | validates CloudWatch default alarms count | unit | cloudwatch | pass |
| TestDefaultAlarms_Namespace | validates CloudWatch default alarms namespace | unit | cloudwatch | pass |
| TestDefaultAlarms_SNSTopic | validates CloudWatch default alarms SNS topic | unit | cloudwatch | pass |
| TestDefaultAlarms_MetricNames | validates CloudWatch default alarms metric names | unit | cloudwatch | pass |
| TestDefaultAlarms_Severities | validates CloudWatch default alarms severities | unit | cloudwatch | pass |
| TestBatcher_AddCounter | validates CloudWatch batcher add counter | unit | cloudwatch | pass |
| TestBatcher_AddHistogram | validates CloudWatch batcher add histogram | unit | cloudwatch | pass |
| TestBatcher_DefaultTags | validates CloudWatch batcher default tags | unit | cloudwatch | pass |
| TestBatcher_BufferFull | validates CloudWatch batcher buffer full | unit | cloudwatch | pass |
| TestBatcher_DrainClears | validates CloudWatch batcher drain clears | unit | cloudwatch | pass |
| TestBatcher_DimensionLimit | validates CloudWatch batcher dimension limit | unit | cloudwatch | pass |
| TestBatcher_MultipleHistogramKeys | validates CloudWatch batcher multiple histogram keys | unit | cloudwatch | pass |
| TestConfig_Defaults | validates CloudWatch config defaults | unit | cloudwatch | pass |
| TestOptions | validates CloudWatch options | unit | cloudwatch | pass |
| TestMetricNameFromKey | validates CloudWatch metric name from key | unit | cloudwatch | pass |
| TestBatcher_HistogramUnit | validates CloudWatch batcher histogram unit | unit | cloudwatch | pass |
| TestBridgeFactory_NewSession_ReturnsNilNil | validates ASB bridge factory session returns nil | unit | servicebus | pass |
| TestBridgeFactory_Capabilities | validates ASB bridge factory capabilities | unit | servicebus | pass |
| TestBridgeFactory_NewReceiver | validates ASB bridge factory new receiver | unit | servicebus | pass |
| TestBridgeFactory_NewSender | validates ASB bridge factory new sender | unit | servicebus | pass |
| TestBridgeFactory_NewReceiver_TopicSubscription | validates ASB bridge factory topic subscription | unit | servicebus | pass |
| TestBridgeFactory_NewSender_Topic | validates ASB bridge factory sender topic | unit | servicebus | pass |
| TestReceiverFactory_NewReceiver | validates ASB receiver factory | unit | servicebus | pass |
| TestSenderFactory_NewSender | validates ASB sender factory | unit | servicebus | pass |
| TestReceiverConfig_Validate | validates ASB receiver config validation | unit | servicebus | pass |
| TestReceiverConfig_ApplyDefaults | validates ASB receiver config defaults | unit | servicebus | pass |
| TestReceiverConfig_AutoExtendEnabled | validates ASB receiver auto-extend enabled | unit | servicebus | pass |
| TestSenderConfig_Validate | validates ASB sender config validation | unit | servicebus | pass |
| TestSenderConfig_ApplyDefaults | validates ASB sender config defaults | unit | servicebus | pass |
| TestReceiverConfigFromOptions | validates ASB receiver config from options | unit | servicebus | pass |
| TestSenderConfigFromOptions | validates ASB sender config from options | unit | servicebus | pass |
| TestDelivery_Envelope | validates ASB delivery envelope | unit | servicebus | pass |
| TestDelivery_Ack_CompletesMessage | validates ASB delivery ack completes | unit | servicebus | pass |
| TestDelivery_Ack_Error | validates ASB delivery ack error | unit | servicebus | pass |
| TestDelivery_Retry_AbandonsMessage | validates ASB delivery retry abandons | unit | servicebus | pass |
| TestDelivery_Retry_Error | validates ASB delivery retry error | unit | servicebus | pass |
| TestDelivery_Extend_RenewsLock | validates ASB delivery extend renews lock | unit | servicebus | pass |
| TestDelivery_Extend_Error | validates ASB delivery extend error | unit | servicebus | pass |
| TestDelivery_AutoExtend_CallsRenew | validates ASB auto-extend calls renew | unit | servicebus | pass |
| TestDelivery_AutoExtend_StopsOnAck | validates ASB auto-extend stops on ack | unit | servicebus | pass |
| TestDelivery_AutoExtend_StopsOnRetry | validates ASB auto-extend stops on retry | unit | servicebus | pass |
| TestDelivery_AutoExtend_StopsOnError | validates ASB auto-extend stops on error | unit | servicebus | pass |
| TestDelivery_NoAutoExtend | validates ASB no auto-extend | unit | servicebus | pass |
| TestDelivery_MultipleStopsAreSafe | validates ASB multiple stops are safe | unit | servicebus | pass |
| TestDelivery_AutoExtend_UsesLockedUntil | validates ASB auto-extend uses locked until | unit | servicebus | pass |
| TestMapError_Nil | validates ASB map error nil | unit | servicebus | pass |
| TestMapError_ContextDeadline | validates ASB map error context deadline | unit | servicebus | pass |
| TestMapError_ContextCanceled | validates ASB map error context canceled | unit | servicebus | pass |
| TestMapError_MessageTooLarge | validates ASB map error message too large | unit | servicebus | pass |
| TestMapError_ServiceBusCodeTimeout | validates ASB map error timeout code | unit | servicebus | pass |
| TestMapError_ServiceBusCodeConnectionLost | validates ASB map error connection lost | unit | servicebus | pass |
| TestMapError_ServiceBusCodeLockLost | validates ASB map error lock lost | unit | servicebus | pass |
| TestMapError_ServiceBusCodeUnauthorizedAccess | validates ASB map error unauthorized | unit | servicebus | pass |
| TestMapError_StringPatterns | validates ASB map error string patterns | unit | servicebus | pass |
| TestMapError_IsRecoverable | validates ASB map error is recoverable | unit | servicebus | pass |
| TestMessageToHeaders_SystemProperties | validates ASB message to headers system props | unit | servicebus | pass |
| TestMessageToHeaders_NilPointers | validates ASB message to headers nil pointers | unit | servicebus | pass |
| TestMessageToHeaders_ApplicationProperties | validates ASB message to headers app props | unit | servicebus | pass |
| TestMessageToHeaders_StripsReservedHeaders | validates ASB message strips reserved headers | unit | servicebus | pass |
| TestMessageToHeaders_MixedProperties | validates ASB message mixed properties | unit | servicebus | pass |
| TestHeadersToMessage_SystemProperties | validates ASB headers to message system props | unit | servicebus | pass |
| TestHeadersToMessage_ApplicationProperties | validates ASB headers to message app props | unit | servicebus | pass |
| TestHeadersToMessage_ExcludesASBHeaders | validates ASB headers excludes ASB headers | unit | servicebus | pass |
| TestHeadersToMessage_NilHeaders | validates ASB headers to message nil | unit | servicebus | pass |
| TestHeadersToMessage_TTLRoundTrip | validates ASB headers TTL round trip | unit | servicebus | pass |
| TestNewSender_Validation | validates ASB sender validation | unit | servicebus | pass |
| TestSender_Send_Success | validates ASB sender send success | unit | servicebus | pass |
| TestSender_Send_HeaderMapping | validates ASB sender header mapping | unit | servicebus | pass |
| TestSender_Send_DefaultSessionID | validates ASB sender default session ID | unit | servicebus | pass |
| TestSender_Send_HeaderSessionOverride | validates ASB sender header session override | unit | servicebus | pass |
| TestSender_Send_Error | validates ASB sender send error | unit | servicebus | pass |
| TestSender_Send_SubjectMapping | validates ASB sender subject mapping | unit | servicebus | pass |
| TestSender_Send_EmptyID | validates ASB sender empty ID | unit | servicebus | pass |
| TestSender_Send_NilHeaders | validates ASB sender nil headers | unit | servicebus | pass |
| TestSender_SendBatch_EmptySlice | validates ASB sender batch empty slice | unit | servicebus | pass |
| TestSender_SendBatch_NewBatchError | validates ASB sender batch new batch error | unit | servicebus | pass |
| TestSender_SendBatch_SendBatchError | validates ASB sender batch send error | unit | servicebus | pass |
| TestSender_Close | validates ASB sender close | unit | servicebus | pass |
| TestSender_Close_Error | validates ASB sender close error | unit | servicebus | pass |
| TestSender_Send_ContextCanceled | validates ASB sender context canceled | unit | servicebus | pass |
| TestSender_Send_Timestamps | validates ASB sender timestamps | unit | servicebus | pass |
| TestSender_Send_MultipleMessages | validates ASB sender multiple messages | unit | servicebus | pass |
| TestReceiver_RunEmitsDeliveries | validates ASB receiver emits deliveries | unit | servicebus | pass |
| TestReceiver_RunStripsReservedHeaders | validates ASB receiver strips reserved headers | unit | servicebus | pass |
| TestReceiver_RunRetriesOnReceiveError | validates ASB receiver retries on error | unit | servicebus | pass |
| TestReceiver_RunStopsOnEmitError | validates ASB receiver stops on emit error | unit | servicebus | pass |
| TestReceiver_Validate_RequiresQueue | validates ASB receiver requires queue | unit | servicebus | pass |
| TestReceiver_SubjectFromTopic | validates ASB receiver subject from topic | unit | servicebus | pass |
| TestReceiver_SubjectOverrideFromMessage | validates ASB receiver subject override | unit | servicebus | pass |
| TestReceiver_DeliveryAck_CompletesCorrectMessage | validates ASB delivery ack correct message | unit | servicebus | pass |
| TestMain | validates ASB integration test setup | integration | servicebus | pass |
| TestIntegration_SendReceive | validates ASB send and receive | integration | servicebus | pass |
| TestIntegration_AckRetryExtend | validates ASB ack retry extend | integration | servicebus | pass |
| TestIntegration_BatchSend | validates ASB batch send | integration | servicebus | pass |
| TestIntegration_ErrorMapping | validates ASB error mapping | integration | servicebus | pass |
| TestIntegration_AutoExtend | validates ASB auto-extend | integration | servicebus | pass |
| TestIntegration_TopicSubscription | validates ASB topic subscription | integration | servicebus | pass |
| TestBridgeFactory_Capabilities | validates MQTT bridge factory capabilities | unit | paho | pass |
| TestBridgeFactory_NewSession_Success | validates MQTT bridge factory session success | unit | paho | pass |
| TestBridgeFactory_NewSession_MissingClientID | validates MQTT missing client ID rejection | unit | paho | pass |
| TestBridgeFactory_NewSession_MissingBrokerURLs | validates MQTT missing broker URLs rejection | unit | paho | pass |
| TestBridgeFactory_NewReceiver_Success | validates MQTT bridge factory receiver success | unit | paho | pass |
| TestBridgeFactory_NewReceiver_WrongSessionType | validates MQTT wrong session type rejection | unit | paho | pass |
| TestBridgeFactory_NewSender_Success | validates MQTT bridge factory sender success | unit | paho | pass |
| TestBridgeFactory_NewSender_WrongSessionType | validates MQTT sender wrong session type | unit | paho | pass |
| TestSessionOptionsFromMap_Defaults | validates MQTT session options defaults | unit | paho | pass |
| TestSessionOptionsFromMap_BrokerURLs | validates MQTT session broker URLs | unit | paho | pass |
| TestSessionOptionsFromMap_SingleBrokerURL | validates MQTT session single broker URL | unit | paho | pass |
| TestSessionOptionsFromMap_Auth | validates MQTT session auth options | unit | paho | pass |
| TestSessionOptionsFromMap_TLSFromMap | validates MQTT session TLS from map | unit | paho | pass |
| TestSessionOptionsFromMap_SessionExpiry | validates MQTT session expiry option | unit | paho | pass |
| TestSenderOptionsFromMap_Defaults | validates MQTT sender options defaults | unit | paho | pass |
| TestSenderOptionsFromMap_AllFields | validates MQTT sender options all fields | unit | paho | pass |
| TestSenderOptionsFromMap_InvalidQoS | validates MQTT sender invalid QoS | unit | paho | pass |
| TestDelivery_Envelope | validates MQTT delivery envelope | unit | paho | pass |
| TestDelivery_AckIsNoop | validates MQTT delivery ack is noop | unit | paho | pass |
| TestDelivery_RetryNotSupported | validates MQTT delivery retry not supported | unit | paho | pass |
| TestDelivery_ExtendNotSupported | validates MQTT delivery extend not supported | unit | paho | pass |
| TestMapError_Nil | validates MQTT map error nil | unit | paho | pass |
| TestMapError_DeadlineExceeded | validates MQTT map error deadline exceeded | unit | paho | pass |
| TestMapError_Canceled | validates MQTT map error canceled | unit | paho | pass |
| TestMapError_NetTimeout | validates MQTT map error net timeout | unit | paho | pass |
| TestMapError_NetNonTimeout | validates MQTT map error net non-timeout | unit | paho | pass |
| TestMapError_ConnectionRefused | validates MQTT map error connection refused | unit | paho | pass |
| TestMapError_UnknownFallsToUnavailable | validates MQTT map error unknown falls to unavailable | unit | paho | pass |
| TestMapDisconnectReasonCode | validates MQTT disconnect reason code mapping | unit | paho | pass |
| TestMapPublishReasonCode | validates MQTT publish reason code mapping | unit | paho | pass |
| TestMapSubscribeReasonCode | validates MQTT subscribe reason code mapping | unit | paho | pass |
| TestEnvelopeFromPublish_BasicFields | validates MQTT envelope from publish basic | unit | paho | pass |
| TestEnvelopeFromPublish_CorrelationAndContentType | validates MQTT envelope correlation/content type | unit | paho | pass |
| TestEnvelopeFromPublish_MessageExpiry | validates MQTT envelope message expiry | unit | paho | pass |
| TestEnvelopeFromPublish_UserProperties | validates MQTT envelope user properties | unit | paho | pass |
| TestEnvelopeFromPublish_StripsReservedHeaders | validates MQTT envelope strips reserved headers | unit | paho | pass |
| TestEnvelopeFromPublish_ResponseTopic | validates MQTT envelope response topic | unit | paho | pass |
| TestPublishFromEnvelope_BasicFields | validates MQTT publish from envelope basic | unit | paho | pass |
| TestPublishFromEnvelope_DefaultTopic | validates MQTT publish default topic | unit | paho | pass |
| TestPublishFromEnvelope_Headers | validates MQTT publish from envelope headers | unit | paho | pass |
| TestPublishFromEnvelope_MessageExpiry | validates MQTT publish message expiry | unit | paho | pass |
| TestPublishFromEnvelope_NoProperties | validates MQTT publish no properties | unit | paho | pass |
| TestRoundTrip_EnvelopePublishEnvelope | validates MQTT envelope round trip | unit | paho | pass |
| TestMain | validates MQTT integration test setup | integration | paho | pass |
| TestIntegration_SessionStartAndClose | validates MQTT session start and close | integration | paho | pass |
| TestIntegration_SessionCloseIdempotent | validates MQTT session close idempotent | integration | paho | pass |
| TestIntegration_SessionEvents | validates MQTT session events | integration | paho | pass |
| TestIntegration_SessionReconcile | validates MQTT session reconcile | integration | paho | pass |
| TestIntegration_PubSubRoundTrip | validates MQTT pub/sub round trip | integration | paho | pass |
| TestIntegration_BackpressureNoDrops | validates MQTT backpressure no drops | integration | paho | pass |
| TestIntegration_QoS1Completion | validates MQTT QoS1 completion | integration | paho | pass |
| TestIntegration_Factory | validates MQTT factory integration | integration | paho | pass |
| TestIntegration_SharedSubscription_CompetingConsumers | validates $share/ distributes messages across subscribers | integration | paho | pass |
| TestIntegration_PlainSubscription_FanOut | validates plain topic delivers to ALL subscribers (N-fold) | integration | paho | pass |
| TestIntegration_SharedSubscription_PayloadIntegrity | validates payload and headers survive $share/ delivery | integration | paho | pass |
| TestNew_ValidPath | validates file credential new valid path | unit | file_credentials | pass |
| TestNew_EmptyPath | validates file credential new empty path | unit | file_credentials | pass |
| TestNew_AutoCreatesDirectory | validates file credential auto creates dir | unit | file_credentials | pass |
| TestNew_WithNamespace | validates file credential with namespace | unit | file_credentials | pass |
| TestScheme | validates file credential scheme | unit | file_credentials | pass |
| TestNamespace_Default | validates file credential default namespace | unit | file_credentials | pass |
| TestNamespace_Configured | validates file credential configured namespace | unit | file_credentials | pass |
| TestURIToPath_ValidURIs | validates file credential URI to path valid | unit | file_credentials | pass |
| TestURIToPath_Invalid | validates file credential URI to path invalid | unit | file_credentials | pass |
| TestURIToPath_PathTraversal | validates file credential URI path traversal | unit | file_credentials | pass |
| TestList_PathTraversal | validates file credential list path traversal | unit | file_credentials | pass |
| TestURIToPath_RoundTrip | validates file credential URI round trip | unit | file_credentials | pass |
| TestCreate_Success | validates file credential create success | unit | file_credentials | pass |
| TestCreate_DuplicateRejectsWithAlreadyExists | validates file credential create duplicate | unit | file_credentials | pass |
| TestCreate_NestedDirectoryCreation | validates file credential nested directory | unit | file_credentials | pass |
| TestGet_Success | validates file credential get success | unit | file_credentials | pass |
| TestGet_NotFound | validates file credential get not found | unit | file_credentials | pass |
| TestGet_CorruptedJSON | validates file credential get corrupted JSON | unit | file_credentials | pass |
| TestUpdate_Success | validates file credential update success | unit | file_credentials | pass |
| TestUpdate_VersionIncremented | validates file credential update version | unit | file_credentials | pass |
| TestUpdate_NotFound | validates file credential update not found | unit | file_credentials | pass |
| TestUpdate_VersionMismatch | validates file credential update version mismatch | unit | file_credentials | pass |
| TestUpdate_NoVersionCheck | validates file credential update no version check | unit | file_credentials | pass |
| TestDelete_Success | validates file credential delete success | unit | file_credentials | pass |
| TestDelete_NotFound | validates file credential delete not found | unit | file_credentials | pass |
| TestDelete_VersionMismatch | validates file credential delete version mismatch | unit | file_credentials | pass |
| TestMemoryStoreFactory_NewLeaseStore | validates memory store factory new lease store | unit | native_store | pass |
| TestMemoryStoreFactory_NewOutboxStore | validates memory store factory new outbox store | unit | native_store | pass |
| TestMemoryStoreFactory_NewDLQStore | validates memory store factory new DLQ store | unit | native_store | pass |
| TestSQLiteStoreFactory_NewLeaseStore_ReturnsNil | validates SQLite factory lease returns nil | unit | native_store | pass |
| TestSQLiteStoreFactory_NewOutboxStore | validates SQLite factory new outbox store | unit | native_store | pass |
| TestSQLiteStoreFactory_NewDLQStore | validates SQLite factory new DLQ store | unit | native_store | pass |
| TestSQLiteStoreFactory_MissingPath | validates SQLite factory missing path | unit | native_store | pass |
| TestDLQStoreConformance | validates memory DLQ conformance | unit | memorydlq | pass |
| TestWriteAndList | validates memory DLQ write and list | unit | memorydlq | pass |
| TestListFilterByRouteID | validates memory DLQ list filter by route | unit | memorydlq | pass |
| TestListFilterByCategory | validates memory DLQ list filter by category | unit | memorydlq | pass |
| TestListFilterBySince | validates memory DLQ list filter by since | unit | memorydlq | pass |
| TestListFilterByBefore | validates memory DLQ list filter by before | unit | memorydlq | pass |
| TestListRespectsLimit | validates memory DLQ list respects limit | unit | memorydlq | pass |
| TestListSortedByFailedAtDescending | validates memory DLQ list sort order | unit | memorydlq | pass |
| TestWriteIdempotent | validates memory DLQ write idempotent | unit | memorydlq | pass |
| TestReplayMarksEntries | validates memory DLQ replay marks entries | unit | memorydlq | pass |
| TestReplayNotFound | validates memory DLQ replay not found | unit | memorydlq | pass |
| TestReplayPartialNotFound | validates memory DLQ replay partial not found | unit | memorydlq | pass |
| TestPurgeRemovesOld | validates memory DLQ purge removes old | unit | memorydlq | pass |
| TestPurgeSkipsRecent | validates memory DLQ purge skips recent | unit | memorydlq | pass |
| TestPurgeReturnsZeroWhenEmpty | validates memory DLQ purge empty returns zero | unit | memorydlq | pass |
| TestFullLifecycle | validates memory DLQ full lifecycle | unit | memorydlq | pass |
| TestConcurrentWrite | validates memory DLQ concurrent write | unit | memorydlq | pass |
| TestListEmptyStore | validates memory DLQ list empty store | unit | memorydlq | pass |
| TestConformanceSuite | validates memory lease conformance | unit | memorylease | pass |
| TestAcquireFreshLease | validates memory lease acquire fresh | unit | memorylease | pass |
| TestAcquireAlreadyHeldLease | validates memory lease acquire already held | unit | memorylease | pass |
| TestAcquireExpiredLease | validates memory lease acquire expired | unit | memorylease | pass |
| TestRenewSuccess | validates memory lease renew success | unit | memorylease | pass |
| TestRenewStaleToken | validates memory lease renew stale token | unit | memorylease | pass |
| TestRenewWrongOwner | validates memory lease renew wrong owner | unit | memorylease | pass |
| TestRenewNonExistent | validates memory lease renew non-existent | unit | memorylease | pass |
| TestReleaseSuccess | validates memory lease release success | unit | memorylease | pass |
| TestReleaseStaleToken | validates memory lease release stale token | unit | memorylease | pass |
| TestReleaseNonExistent | validates memory lease release non-existent | unit | memorylease | pass |
| TestCurrentReturnsInfo | validates memory lease current returns info | unit | memorylease | pass |
| TestCurrentNonExistent | validates memory lease current non-existent | unit | memorylease | pass |
| TestConcurrentAcquire | validates memory lease concurrent acquire | unit | memorylease | pass |
| TestVersionMonotonicallyIncreases | validates memory lease version monotonic | unit | memorylease | pass |
| TestOutboxStoreConformance | validates memory outbox conformance | unit | memoryoutbox | pass |
| TestDLQStoreConformance | validates SQLite DLQ conformance | unit | sqlitedlq | pass |
| TestWriteAndList | validates SQLite DLQ write and list | unit | sqlitedlq | pass |
| TestListFilterByRouteID | validates SQLite DLQ list filter by route | unit | sqlitedlq | pass |
| TestListFilterByCategory | validates SQLite DLQ list filter by category | unit | sqlitedlq | pass |
| TestListFilterBySince | validates SQLite DLQ list filter by since | unit | sqlitedlq | pass |
| TestListFilterByBefore | validates SQLite DLQ list filter by before | unit | sqlitedlq | pass |
| TestListRespectsLimit | validates SQLite DLQ list respects limit | unit | sqlitedlq | pass |
| TestWriteIdempotent | validates SQLite DLQ write idempotent | unit | sqlitedlq | pass |
| TestReplayMarksEntries | validates SQLite DLQ replay marks entries | unit | sqlitedlq | pass |
| TestPurgeRemovesOld | validates SQLite DLQ purge removes old | unit | sqlitedlq | pass |
| TestPurgeSkipsRecent | validates SQLite DLQ purge skips recent | unit | sqlitedlq | pass |
| TestFullLifecycle | validates SQLite DLQ full lifecycle | unit | sqlitedlq | pass |
| TestInMemoryMode | validates SQLite DLQ in-memory mode | unit | sqlitedlq | pass |
| TestDurability_CloseAndReopen | validates SQLite DLQ durability close/reopen | unit | sqlitedlq | pass |
| TestFileExistsAfterClose | validates SQLite DLQ file exists after close | unit | sqlitedlq | pass |
| TestListOrderByFailedAtDesc | validates SQLite DLQ list order by failed_at desc | unit | sqlitedlq | pass |
| TestReplayNonExistentEntry | validates SQLite DLQ replay non-existent entry | unit | sqlitedlq | pass |
| TestListEmptyStore | validates SQLite DLQ list empty store | unit | sqlitedlq | pass |
| TestOutboxStoreConformance | validates SQLite outbox conformance | unit | sqliteoutbox | pass |
| TestInMemoryMode | validates SQLite outbox in-memory mode | unit | sqliteoutbox | pass |
| TestDurability_CloseAndReopen | validates SQLite outbox durability close/reopen | unit | sqliteoutbox | pass |
| TestTempFileCleanup | validates SQLite outbox temp file cleanup | unit | sqliteoutbox | pass |
| TestDispatchHeadersRoundTrip | validates SQLite outbox dispatch headers round trip | unit | sqliteoutbox | pass |
| TestExpiresAtRoundTrip | validates SQLite outbox expires-at round trip | unit | sqliteoutbox | pass |
| TestConfig_Defaults | validates OTel metrics config defaults | unit | otel_metrics | pass |
| TestConfig_DefaultsPreserveExplicit | validates OTel metrics defaults preserve explicit | unit | otel_metrics | pass |
| TestOptions | validates OTel metrics options | unit | otel_metrics | pass |
| TestExporter_BuildAttributes | validates OTel metrics build attributes | unit | otel_metrics | pass |
| TestExporter_BuildAttributesEmpty | validates OTel metrics build attributes empty | unit | otel_metrics | pass |
| TestConfig_Defaults | validates OTel tracing config defaults | unit | otel_tracing | pass |
| TestOptions | validates OTel tracing options | unit | otel_tracing | pass |
| TestStartSpan_CreatesSpan | validates OTel tracing start span creates span | unit | otel_tracing | pass |
| TestSpan_SetError | validates OTel tracing span set error | unit | otel_tracing | pass |
| TestSpan_AddEvent | validates OTel tracing span add event | unit | otel_tracing | pass |
| TestSpan_SetAttributes | validates OTel tracing span set attributes | unit | otel_tracing | pass |
| TestGenerate_DefaultECDSA | validates TLS generate default ECDSA | unit | tlsgen | pass |
| TestGenerate_RSA | validates TLS generate RSA | unit | tlsgen | pass |
| TestGenerate_CA | validates TLS generate CA | unit | tlsgen | pass |
| TestGenerate_CustomSANs | validates TLS generate custom SANs | unit | tlsgen | pass |
| TestGenerate_InvalidKeyType | validates TLS generate invalid key type | unit | tlsgen | pass |
| TestMustGenerate_Success | validates TLS must generate success | unit | tlsgen | pass |
| TestMustGenerate_Panics | validates TLS must generate panics | unit | tlsgen | pass |
| TestTestCredentialSet | validates TLS test credential set | unit | tlsgen | pass |
| TestMain | validates E2E shared outbox test setup | e2e | e2e_shared_outbox | pass |
| TestE2E_DynamoDB_SharedOutboxFlow | validates E2E DynamoDB shared outbox flow | e2e | e2e_shared_outbox | pass |
| TestE2E_DynamoDB_LeaseTransfer | validates E2E DynamoDB lease transfer | e2e | e2e_shared_outbox | pass |
| TestE2E_MemoryLease_DynamoOutbox | validates E2E memory lease with DynamoDB outbox | e2e | e2e_shared_outbox | pass |
| TestE2E_DynamoDB_CrashRecovery | validates E2E DynamoDB crash recovery | e2e | e2e_shared_outbox | pass |
| TestE2E_DynamoDB_FencingValidation | validates E2E DynamoDB fencing validation | e2e | e2e_shared_outbox | pass |
| TestE2E_DynamoDB_PoisonMessage | validates E2E DynamoDB poison message | e2e | e2e_shared_outbox | pass |
| TestE2E_DynamoDB_FanOutAtomicity | validates E2E DynamoDB fan-out atomicity | e2e | e2e_shared_outbox | pass |
| TestE2E_S1_SQSToMQTT_DirectHold | validates E2E SQS to MQTT direct-hold | e2e | e2e_sqs_mqtt | pass |
| TestE2E_S2_SQSToMQTT_SharedOutbox | validates E2E SQS to MQTT shared outbox | e2e | e2e_sqs_mqtt | pass |
| TestE2E_S3_MQTTToSQS_DirectHold | validates E2E MQTT to SQS direct-hold | e2e | e2e_sqs_mqtt | pass |
| TestE2E_S4_SQSToMQTT_BridgeCrashAndRestart | validates E2E SQS to MQTT crash and restart | e2e | e2e_sqs_mqtt | pass |
| TestE2E_S5_SQSToMQTT_SecondaryBridgeTakeover | validates E2E secondary bridge takeover | e2e | e2e_sqs_mqtt | pass |
| TestE2E_S6_SQSToMQTT_RoundTrip | validates E2E SQS to MQTT round trip | e2e | e2e_sqs_mqtt | pass |
| TestE2E_S7_SQSToMQTT_MultipleMessages | validates E2E SQS to MQTT multiple messages | e2e | e2e_sqs_mqtt | pass |
| TestE2E_S8_SQSToMQTT_ProcessorChain | validates E2E SQS to MQTT processor chain | e2e | e2e_sqs_mqtt | pass |
| TestE2E_F1_Failover_SingleInstance_CrashBeforeDrain | validates E2E failover single instance crash | e2e | e2e_failover | pass |
| TestE2E_F2_Failover_TwoInstances_LeaseTransfer | validates E2E failover two instances lease transfer | e2e | e2e_failover | pass |
| TestE2E_F3_Failover_ThreeInstances_CascadingFailure | validates E2E failover cascading failure | e2e | e2e_failover | pass |
| TestE2E_F4_Failover_ThreeInstances_StaleFencingToken | validates E2E failover stale fencing token | e2e | e2e_failover | pass |
| TestE2E_F5_Failover_ConnectAfterLease | validates E2E failover connect after lease | e2e | e2e_failover | pass |
| TestE2E_F6_Failover_FanOutCrossInstance_ThreeSessions | validates E2E failover fan-out three sessions | e2e | e2e_failover | pass |
| TestE2E_F7_Failover_FanOutSessionOwnerCrash | validates E2E failover fan-out owner crash | e2e | e2e_failover | pass |
| TestE2E_F8_Failover_IngressCrashSQSRedelivery | validates E2E failover ingress crash redelivery | e2e | e2e_failover | pass |
| TestE2E_F9_Failover_MultiMessage_ThreeInstances | validates E2E failover multi-message three instances | e2e | e2e_failover | pass |
| TestE2E_F10_Failover_GracefulStepDown | validates E2E failover graceful step down | e2e | e2e_failover | pass |
| TestE2E_Routing_FanOutMatchAll_ThreeClients | validates E2E routing fan-out match all | e2e | e2e_routing | pass |
| TestE2E_Routing_MatchByHeader_SelectsCorrectClient | validates E2E routing match by header | e2e | e2e_routing | pass |
| TestE2E_Routing_MatchByHeader_EachClientGetsOwnMessage | validates E2E routing each client gets own msg | e2e | e2e_routing | pass |
| TestE2E_Routing_AddressTemplate_DynamicTopic | validates E2E routing address template dynamic | e2e | e2e_routing | pass |
| TestE2E_Routing_AddressTemplate_MultiPlaceholder | validates E2E routing multi placeholder template | e2e | e2e_routing | pass |
| TestE2E_Routing_FanOutSameSession_DifferentTopics | validates E2E routing fan-out same session | e2e | e2e_routing | pass |
| TestE2E_Routing_FanOutPartialAvailability | validates E2E routing fan-out partial availability | e2e | e2e_routing | pass |
| TestE2E_Routing_NoMatchingBinding_DLQ | validates E2E routing no matching binding DLQ | e2e | e2e_routing | pass |
| TestE2E_Routing_MissingTemplatePlaceholder_DLQ | validates E2E routing missing placeholder DLQ | e2e | e2e_routing | pass |
| TestE2E_Routing_FanOutToFiveClients_TenMessages | validates E2E routing fan-out five clients | e2e | e2e_routing | pass |
| TestE2E_MQTTToSQS_SingleTopic | validates E2E MQTT to SQS single topic | e2e | e2e_mqtt_to_sqs | pass |
| TestE2E_MQTTToSQS_MultiTopicMerge | validates E2E MQTT to SQS multi topic merge | e2e | e2e_mqtt_to_sqs | pass |
| TestE2E_MQTTToSQS_HeaderBasedRouting | validates E2E MQTT to SQS header routing | e2e | e2e_mqtt_to_sqs | pass |
| TestE2E_MQTTToSQS_RoundTripWithFailover | validates E2E MQTT to SQS round trip failover | e2e | e2e_mqtt_to_sqs | pass |
| TestE2E_MQTTToSQS_BackpressureSQSSlow | validates E2E MQTT to SQS backpressure | e2e | e2e_mqtt_to_sqs | pass |
| TestLeaseToken_ZeroValue | validates zero-value LeaseToken semantics | unit | domain | pass |
| TestLeaseInfo_ZeroValue | validates zero-value LeaseInfo semantics | unit | domain | pass |
| TestLeaseInfo_Fields | validates LeaseInfo field assignment | unit | domain | pass |
| TestCredentialKind_Constants | validates credential kind constants are distinct | unit | domain | pass |
| TestCredentialSet_ZeroValue | validates zero-value CredentialSet has nil fields | unit | domain | pass |
| TestPasswordCredential_Accessors | validates PasswordCredential constructor and accessors | unit | domain | pass |
| TestTLSMaterial_Accessors | validates TLSMaterial constructor and accessors | unit | domain | pass |
| TestMetricNamespace_NonEmpty | validates MetricNamespace is non-empty | unit | domain | pass |
| TestMetricConstants_NonEmpty | validates all Metric constants are non-empty and unique | unit | domain | pass |
| TestTagKeyConstants_NonEmpty | validates all TagKey constants are non-empty and unique | unit | domain | pass |
| TestTag_Construction | validates Tag struct creation | unit | domain | pass |
| TestOutboxPartitionKey_WithSession | validates SESSION# partition key format | unit | domain | pass |
| TestOutboxPartitionKey_WithBinding | validates BINDING# partition key format | unit | domain | pass |
| TestOutboxPartitionKey_Deterministic | validates partition key determinism | unit | domain | pass |
| TestDefaultBackoffPolicy_Values | validates default backoff configuration | unit | domain | pass |
| TestDeliveryMode_Constants | validates DeliveryMode enum values are distinct | unit | domain | pass |
| TestDispatchMode_Constants | validates DispatchMode enum values are distinct | unit | domain | pass |
| TestSessionMode_Constants | validates SessionMode enum values are distinct | unit | domain | pass |
| TestAckBoundary_Constants | validates AckBoundary enum values are distinct | unit | domain | pass |
| TestExpiredAction_Constants | validates ExpiredAction enum values are distinct | unit | domain | pass |
| TestFailureAction_Constants | validates FailureAction enum values are distinct | unit | domain | pass |
| TestReceiverConfigFromOptions_Defaults | validates SQS receiver config defaults | unit | sqs_config | pass |
| TestReceiverConfigFromOptions_AllFields | validates SQS receiver config all fields | unit | sqs_config | pass |
| TestSenderConfigFromOptions_Defaults | validates SQS sender config defaults | unit | sqs_config | pass |
| TestSenderConfigFromOptions_AllFields | validates SQS sender config all fields | unit | sqs_config | pass |
| TestSenderConfigFromOptions_FIFODetection | validates SQS FIFO detection logic | unit | sqs_config | pass |
| TestReceiverFactory_NewReceiver_OptionsPassthrough | validates SQS receiver factory options passthrough | unit | sqs_factory | pass |
| TestSenderFactory_NewSender_OptionsPassthrough | validates SQS sender factory options passthrough | unit | sqs_factory | pass |
| TestFactory_NewSession_MissingClientID | validates MQTT factory rejects missing client_id | unit | mqtt_factory | pass |
| TestFactory_NewSession_MissingBrokerURLs | validates MQTT factory rejects missing broker URLs | unit | mqtt_factory | pass |
| TestFactory_NewSession_ValidOptions | validates MQTT factory creates session with valid options | unit | mqtt_factory | pass |
| TestFactory_NewReceiver_WrongSessionType | validates MQTT factory rejects wrong session type | unit | mqtt_factory | pass |
| TestFactory_NewSender_WrongSessionType | validates MQTT factory rejects wrong session type | unit | mqtt_factory | pass |
| TestReceiverOptionsFromMap_Defaults | validates MQTT receiver options defaults | unit | mqtt_config | pass |
| TestReceiverOptionsFromMap_NonNilMap | validates MQTT receiver options with map | unit | mqtt_config | pass |
| TestHandleStart_StartsRuntime | validates admin start handler starts bridge | unit | httpapi_admin | pass |
| TestHandleStart_AlreadyRunning | validates admin start handler when already running | unit | httpapi_admin | pass |
| TestHandleStop_StopsRuntime | validates admin stop handler stops bridge | unit | httpapi_admin | pass |
| TestHandleStop_NotRunning | validates admin stop handler when not running | unit | httpapi_admin | pass |
| TestHandleDLQMessages_NoStore | validates DLQ messages returns 404 without store | unit | httpapi_admin | pass |
| TestHandleStart_MethodNotAllowed | validates admin start rejects GET method | unit | httpapi_admin | pass |
| TestHandleHealth_WithComponentErrors | validates health endpoint with component errors | unit | httpapi_monitor | pass |
| TestHandleLogs_ReturnsNotImplemented | validates logs endpoint returns stub response | unit | httpapi_monitor | pass |
| TestHandleLive_ReturnsAlive | validates liveness probe returns alive | unit | httpapi_monitor | pass |
| TestMonitorHandleReady_NotRunning | validates readiness probe when not running | unit | httpapi_monitor | pass |
| TestRecoverMW_PanicsReturn500 | validates panic recovery returns HTTP 500 | unit | httpapi_server | pass |
| TestRequestLogMW_LogsRequestDetails | validates request logging middleware output | unit | httpapi_server | pass |
| TestServer_Stop_GracefulShutdown | validates server graceful shutdown | unit | httpapi_server | pass |
| TestServer_Stop_NotRunning | validates server stop when not running | unit | httpapi_server | pass |
| TestInstrumentedOutboxStore_ExpireDelegates | validates Expire delegates to inner store | unit | runtime_instrumented | pass |
| TestValidationError_Errors_ReturnsAllErrors | validates Errors returns copy of all messages | unit | runtime_validator | pass |
| TestBuildTLSConfig_Nil | validates nil TLS config when no settings | unit | servicebus_client | pass |
| TestBuildTLSConfig_InsecureSkipVerify | validates InsecureSkipVerify propagation | unit | servicebus_client | pass |
| TestBuildTLSConfig_WithCACert | validates CA PEM loading into RootCAs | unit | servicebus_client | pass |
| TestBuildClientOptions_NoTLS | validates nil client options without TLS | unit | servicebus_client | pass |
| TestBuildClientOptions_WithTLSConfig | validates client options with explicit TLS | unit | servicebus_client | pass |
| TestWatcher_DebounceCoalesces | validates rapid writes produce single event | unit | file_watcher | pass |
| TestWatcher_InvalidContent | validates corrupt YAML is handled gracefully | unit | file_watcher | pass |
| TestWatcher_WithFormat | validates explicit format override | unit | file_watcher | pass |
| TestTracer_Close_FlushesPendingSpans | validates Close shuts down provider cleanly | unit | otel_tracing | pass |
| TestToRoutePolicy_FieldMapping | validates RouteDef to RoutePolicy field mapping | unit | bridge_convert | pass |
| TestToRoutePolicy_BackoffDurations | validates backoff duration string parsing | unit | bridge_convert | pass |
| TestToSessionConfig_FromRouteSessionDef | validates session config field mapping | unit | bridge_convert | pass |
| TestToSessionConfig_NilReturnsNil | validates nil input returns nil | unit | bridge_convert | pass |
| TestToDrainStrategy_FixedPoll | validates fixed poll drain strategy | unit | bridge_convert | pass |
| TestToDrainStrategy_AdaptiveBackoff | validates adaptive backoff drain strategy | unit | bridge_convert | pass |
| TestIntegration_SQS_Receiver_ReceivesMessages | validates SQS receiver emits deliveries | integration | sqs_integration | pass |
| TestIntegration_SQS_Sender_SendsMessage | validates SQS sender sends message to queue | integration | sqs_integration | pass |
| TestIntegration_SQS_Sender_FIFO | validates SQS FIFO queue message ordering | integration | sqs_integration | pass |
| TestIntegration_SQS_Receiver_VisibilityTimeout | validates SQS message redelivery after timeout | integration | sqs_integration | pass |
| TestIntegration_SQS_Receiver_ContextCancel | validates SQS receiver stops on context cancel | integration | sqs_integration | pass |
| TestDirectStrategy_PassThrough | validates direct strategy passes every config through immediately | unit | reconfig_strategy | pass |
| TestDirectStrategy_MultipleChanges | validates direct strategy emits N changes in order | unit | reconfig_strategy | pass |
| TestDirectStrategy_ContextCancel | validates direct strategy closes output on context cancel | unit | reconfig_strategy | pass |
| TestDirectStrategy_InputChannelClosed | validates direct strategy closes output when input closes | unit | reconfig_strategy | pass |
| TestDebouncedStrategy_QuietWindow | validates debounced strategy emits after quiet period | unit | reconfig_strategy | pass |
| TestDebouncedStrategy_EmitsLatest | validates debounced strategy emits only latest of rapid configs | unit | reconfig_strategy | pass |
| TestDebouncedStrategy_ResetOnNewChange | validates debounced timer resets on each new config | unit | reconfig_strategy | pass |
| TestDebouncedStrategy_ExactlyOneEmitPerBurst | validates burst of 10 changes produces exactly 1 emit | unit | reconfig_strategy | pass |
| TestDebouncedStrategy_ContextCancel_PendingTimer | validates cancel with pending timer emits nothing | unit | reconfig_strategy | pass |
| TestDebouncedStrategy_InputChannelClosed_PendingTimer | validates input close with pending timer emits last | unit | reconfig_strategy | pass |
| TestDebouncedStrategy_ZeroQuietPeriod | validates zero quiet period degrades to pass-through | unit | reconfig_strategy | pass |
| TestWindowedStrategy_QuietWindow | validates windowed strategy emits via quiet window | unit | reconfig_strategy | pass |
| TestWindowedStrategy_MaxDelay | validates windowed strategy forces emit at max delay | unit | reconfig_strategy | pass |
| TestWindowedStrategy_MaxDelayResetAfterEmit | validates max-delay timer resets after forced emit | unit | reconfig_strategy | pass |
| TestWindowedStrategy_QuietBeforeMaxDelay | validates quiet window fires before max delay | unit | reconfig_strategy | pass |
| TestWindowedStrategy_ContextCancel | validates windowed strategy closes output on cancel | unit | reconfig_strategy | pass |
| TestWindowedStrategy_MaxDelayLessThanQuiet | validates maxDelay less than quietPeriod takes precedence | unit | reconfig_strategy | pass |
| TestWindowedStrategy_EqualPeriods | validates equal periods produce deterministic behavior | unit | reconfig_strategy | pass |
| TestSupervisor_InitialBuildAndStart | validates initial config produces running runtime | unit | supervisor | pass |
| TestSupervisor_InitialBuildFailure | validates bad initial config returns error from Run | unit | supervisor | pass |
| TestSupervisor_InitialStartFailure | validates build succeeds but Start fails returns error | unit | supervisor | pass |
| TestSupervisor_RuntimeAccessorBeforeRun | validates Runtime() returns nil before Run | unit | supervisor | pass |
| TestSupervisor_OverlapSwap | validates overlap swap builds new while old runs | unit | supervisor | pass |
| TestSupervisor_OverlapSwap_OldRuntimeStopsCleanly | validates old runtime stops after overlap swap | unit | supervisor | pass |
| TestSupervisor_OverlapSwap_NewRuntimeGetsNewRoutes | validates new runtime has correct routes after swap | unit | supervisor | pass |
| TestSupervisor_PrepareCommitSwap | validates prepare-commit swap ordering | unit | supervisor | pass |
| TestSupervisor_PrepareCommitSwap_SessionsNotCreatedDuringPrepare | validates sessions not created during Prepare | unit | supervisor | pass |
| TestSupervisor_PrepareCommitSwap_SessionsCreatedAfterOldStops | validates sessions created after old runtime stops | unit | supervisor | pass |
| TestSupervisor_AutoDetect | validates auto swap mode detection from transport capabilities | unit | supervisor | pass |
| TestSupervisor_AutoDetect_ConfigChangeRemovesExclusive | validates mode switches when exclusive sessions removed | unit | supervisor | pass |
| TestSupervisor_MultipleConfigChanges | validates 3 sequential configs produce correct runtimes | unit | supervisor | pass |
| TestSupervisor_RapidConfigChanges_WithDirectStrategy | validates 5 rapid changes all applied with direct strategy | unit | supervisor | pass |
| TestSupervisor_RapidConfigChanges_WithDebouncedStrategy | validates 5 rapid changes debounced to 1 | unit | supervisor | pass |
| TestSupervisor_AlternatingValidInvalid | validates valid configs applied, invalid rejected | unit | supervisor | pass |
| TestSupervisor_ConfigRollback_AfterFailure | validates recovery after build failure | unit | supervisor | pass |
| TestSupervisor_SwapCallback_Success | validates SwapEvent emitted on successful swap | unit | supervisor | pass |
| TestSupervisor_SwapCallback_BuildFailure | validates SwapEvent error on build failure | unit | supervisor | pass |
| TestSupervisor_NoSwapCallback_WhenNoneSet | validates no panic when onSwap is nil | unit | supervisor | pass |
| TestSupervisor_ContextCancellation | validates clean shutdown on context cancel | unit | supervisor | pass |
| TestSupervisor_ChannelClosed_GracefulShutdown | validates graceful shutdown when channel closes | unit | supervisor | pass |
| TestSupervisor_NilChangesChannel | validates nil changes channel serves until cancel | unit | supervisor | pass |
| TestSupervisor_EmptyConfig_Rejected | validates empty config fails validation | unit | supervisor | pass |
| TestSupervisor_OverlapBuildFailure_KeepsOldRunning | validates old runtime stays on build failure | unit | supervisor_failure | pass |
| TestSupervisor_OverlapBuildFailure_SwapEventHasError | validates SwapEvent error on overlap build failure | unit | supervisor_failure | pass |
| TestSupervisor_OverlapBuildFailure_NextValidConfigWorks | validates recovery after overlap build failure | unit | supervisor_failure | pass |
| TestSupervisor_PrepareFailure_KeepsOldRunning | validates old runtime stays on prepare failure | unit | supervisor_failure | pass |
| TestSupervisor_PrepareFailure_NextValidConfigWorks | validates recovery after prepare failure | unit | supervisor_failure | pass |
| TestSupervisor_CompleteFailure_AfterStop | validates bridge down after complete failure | unit | supervisor_failure | pass |
| TestSupervisor_CompleteFailure_SwapEventReportsDegraded | validates SwapEvent reports degraded on complete failure | unit | supervisor_failure | pass |
| TestSupervisor_CompleteFailure_NextConfigRecovers | validates recovery after complete failure | unit | supervisor_failure | pass |
| TestSupervisor_StartFailure_Overlap | validates bridge down after overlap start failure | unit | supervisor_failure | pass |
| TestSupervisor_StartFailure_PrepareCommit | validates bridge down after PrepareCommit start failure | unit | supervisor_failure | pass |
| TestSupervisor_StartFailure_NextConfigRecovers | validates recovery after start failure | unit | supervisor_failure | pass |
| TestSupervisor_StopTimeout_Overlap | validates swap proceeds despite stop timeout | unit | supervisor_failure | pass |
| TestSupervisor_StopTimeout_PrepareCommit | validates PrepareCommit proceeds despite stop timeout | unit | supervisor_failure | pass |
| TestSupervisor_FailingSessionClose | validates swap proceeds despite session close error | unit | supervisor_failure | pass |
| TestSupervisor_StopErrorDoesNotPreventSwap | validates stop errors do not prevent swap | unit | supervisor_failure | pass |
| TestSupervisor_BrokerUnreachable_Overlap | validates old runtime stays when broker unreachable | unit | supervisor_failure | pass |
| TestSupervisor_BrokerUnreachable_PrepareCommit | validates bridge down when broker unreachable in commit | unit | supervisor_failure | pass |
| TestSupervisor_NoTransportsRegistered | validates build fails without registered transports | unit | supervisor_failure | pass |
| TestSupervisor_SwapCallback_NotCalledOnInvalidConfig | validates SwapEvent error on invalid config | unit | supervisor_failure | pass |
| TestSupervisor_RuntimeAccessor_DuringSwap | validates concurrent Runtime() access during swap | unit | supervisor_failure | pass |
| TestSupervisor_ConfigAccessor_DuringSwap | validates concurrent Config() access during swap | unit | supervisor_failure | pass |
| TestSupervisor_ConcurrentApplySerializes | validates concurrent config changes serialize | unit | supervisor_failure | pass |
| TestSupervisor_ContextCancel_DuringSwap | validates cancel during swap returns cleanly | unit | supervisor_failure | pass |
| TestSupervisor_ChannelClosed_WhileApplying | validates channel close during apply completes | unit | supervisor_failure | pass |
| TestBuilder_PrepareComplete_EquivalentToBuild | validates Prepare+Complete equivalent to Build | unit | builder_prepare | pass |
| TestBuilder_PrepareComplete_DirectHold | validates Prepare+Complete for direct_hold route | unit | builder_prepare | pass |
| TestBuilder_PrepareComplete_SharedOutbox | validates Prepare+Complete for shared_outbox route | unit | builder_prepare | pass |
| TestBuilder_PrepareFailsOnInvalidConfig | validates Prepare fails on invalid config | unit | builder_prepare | pass |
| TestBuilder_PrepareFailsOnMissingStoreFactory | validates Prepare fails on missing store factory | unit | builder_prepare | pass |
| TestBuilder_PrepareBuildsStores | validates Prepare builds stores successfully | unit | builder_prepare | pass |
| TestBuilder_PrepareDoesNotCallTransportFactory | validates Prepare does not call transport factory methods | unit | builder_prepare | pass |
| TestBuilder_Prepare_ClusteredNonDistributedStore_Rejected | validates clustered mode rejects non-distributed store | unit | builder_prepare | pass |
| TestBuilder_CompleteCreatesSessionsAndRoutes | validates Complete creates sessions and wires routes | unit | builder_prepare | pass |
| TestBuilder_CompleteFailsOnSessionCreationError | validates Complete fails on session creation error | unit | builder_prepare | pass |
| TestBuilder_CompleteNilPrepared | validates Complete returns error for nil PreparedBuild | unit | builder_prepare | pass |
| TestF2_StopReleasesLeaseWithValidContext | validates Stop uses fresh context for lease release when stop context is expired | unit | runtime_resilience | pass |
| TestF3_DrainOnShutdown | validates final drain sweep sends pending records during shutdown | unit | runtime_resilience | pass |
| TestF3_DrainOnShutdown_NoLease | validates no final drain when lease is not held | unit | runtime_resilience | pass |
| TestF4_DirectHoldSharedConsumerRejected | validates validator rejects direct_hold with shared consumer source | unit | runtime_resilience | pass |
| TestF4_DirectHoldAllowUnfenced | validates AllowUnfenced bypasses shared consumer fencing check | unit | runtime_resilience | pass |
| TestF5_DrainBatchSkipsTOCTOUCheck | validates drainBatch does not call leaseStore.Current() before Claim | unit | runtime_resilience | pass |
| TestF6_StaleFencingTokenDoesNotKillRuntime | validates stale fencing token on one drainer does not kill runtime | unit | runtime_resilience | pass |
| TestF6_CriticalErrorStillKillsRuntime | validates non-fencing errors still mark runtime unhealthy | unit | runtime_resilience | pass |
| TestF7_ReacquiredLeaseRestartsDeadSession | validates session.Start re-called when health shows disconnected on re-acquisition | unit | runtime_resilience | pass |
| TestF7_StepDownClosesSessionAndReacquireRestarts | validates step-down closes the source session (no split-brain) and re-acquire restarts it | unit | runtime_resilience | pass |
| TestDrainBatch_ParallelSends | validates records in a drain batch are sent concurrently | unit | outbox_drainer_concurrent | pass |
| TestDrainBatch_ConcurrencyLimit | validates MaxDrainConcurrency caps simultaneous goroutines | unit | outbox_drainer_concurrent | pass |
| TestDrainBatch_ErrorIsolation | validates one record failure does not block others | unit | outbox_drainer_concurrent | pass |
| TestDrainBatch_ConcurrencyDefault | validates default MaxDrainConcurrency is 10 | unit | outbox_drainer_concurrent | pass |
| TestAdaptBatchSize_ScalesUpOnFullBatch | validates batch size doubles when full batch returned | unit | outbox_drainer_concurrent | pass |
| TestAdaptBatchSize_CapsAtMaxBatchSize | validates batch size never exceeds MaxBatchSize | unit | outbox_drainer_concurrent | pass |
| TestAdaptBatchSize_ResetsOnEmptyBatch | validates batch size resets to initial on empty drain | unit | outbox_drainer_concurrent | pass |
| TestAdaptBatchSize_DefaultMaxBatchSize | validates default MaxBatchSize is applied | unit | outbox_drainer_concurrent | pass |
| TestRouteRunner_ConcurrentDelivery | validates MaxInFlight effectively limits concurrent processing | unit | route_runner_concurrent | pass |
| TestRouteRunner_BackpressureOnSemFull | validates emit blocks when MaxInFlight reached | unit | route_runner_concurrent | pass |
| TestRouteRunner_GracefulShutdownWaitsInFlight | validates Run waits for in-flight goroutines on shutdown | unit | route_runner_concurrent | pass |
| TestGlobalSemaphore_LimitsCrossRoute | validates global semaphore limits total cross-route concurrency | unit | route_runner_concurrent | pass |
| TestGlobalSemaphore_ZeroDisablesGlobal | validates no global limit when not configured | unit | route_runner_concurrent | pass |
| TestDepthCache_PreventsRepeatedQueries | validates cache prevents repeated QueryPending calls | unit | outbox_depth_cache | pass |
| TestDepthCache_ExpiresAfterTTL | validates cache expires and triggers new query | unit | outbox_depth_cache | pass |
| TestDepthCache_AtCapacityCachedImmediately | validates at-capacity status is cached | unit | outbox_depth_cache | pass |
| TestOutboxDrainer_CancelDuringBatch_ReturnsPromptly | validates drainBatch exits promptly on mid-batch context cancellation (T1 regression) | unit | outbox_drainer | pass |
| TestOutboxDrainer_CancelBeforeBatch_ExitsPromptly | validates Run exits promptly with pre-cancelled context (T1 regression) | unit | outbox_drainer | pass |
| TestOutboxDrainer_ConcurrentBatch_SemaphoreConsistency | validates concurrent semaphore consistency on cancellation (T1 regression) | unit | outbox_drainer | pass |
| TestPushEvent_ConcurrentClose_NoPanic | validates concurrent pushEvent and Close does not panic (T2 regression) | unit | session_pushevent | pass |
| TestPushEvent_AfterClose_IsNoop | validates pushEvent after Close is a silent no-op (T2 regression) | unit | session_pushevent | pass |
| TestPushEvent_BufferFull_DropsOldest | validates pushEvent drops oldest event when buffer is full (T2 regression) | unit | session_pushevent | pass |
| TestDepthCache_EvictionClearsOnBurst | validates depth cache clears map when burst exceeds maxEntries (T9) | unit | medium_fixes | pass |
| TestDepthCacheTTL_WiredFromPolicy | validates DepthCacheTTL from RoutePolicy reaches RouteRunner (T10) | unit | medium_fixes | pass |
| TestDrainConfig_WiredFromSessionConfig | validates DrainMaxBatchSize/DrainMaxConcurrency wired from SessionConfig (T11) | unit | medium_fixes | pass |
| TestQueryPendingError_FailsClosed | validates QueryPending error fails delivery instead of bypassing (T12) | unit | medium_fixes | pass |
| TestQueryPendingSuccess_PersistsNormally | validates successful QueryPending allows normal outbox persist (T12) | unit | medium_fixes | pass |
| TestAbsoluteMaxBatchSize_Clamps | validates absoluteMaxBatchSize clamps excessive MaxBatchSize (T13) | unit | medium_fixes | pass |
| TestNormalMaxBatchSize_NotClamped | validates reasonable MaxBatchSize preserved without clamping (T13) | unit | medium_fixes | pass |
| TestOutboxDrainer_EmitsRecordFailureMetric | validates MetricOutboxRecordFailures emitted on record failure (T14) | unit | medium_fixes | pass |
| TestOutboxDrainer_SuccessEmitsCompletion | validates successful send emits completion, not failure metric (T14) | unit | medium_fixes | pass |
| TestAdaptBatchSize_HalvesOnConsecutiveZeroSuccess | validates batch size halves on consecutive zero-success cycles (T16) | unit | medium_fixes | pass |
| TestSessionManager_LogsLeaseReleaseError | validates lease release error logged in SessionManager.Close (T17) | unit | medium_fixes | pass |
| TestFakeProcessor_AtomicCalled | validates FakeProcessor.CalledCount is atomic-safe under concurrency (T18) | unit | medium_fixes | pass |
| TestBatchSizeClamped_PreventsAbsoluteMaxBypass | validates BatchSize clamped to prevent absoluteMaxBatchSize bypass (T13) | unit | medium_fixes | pass |
| TestOutboxDrainer_StaleFencingToken_NoRecordFailureMetric | validates stale token does not emit RecordFailures metric (T14) | unit | medium_fixes | pass |
| TestDefaultSessionConfig_IncludesDrainDefaults | validates DefaultSessionConfig includes drain field defaults (T11) | unit | medium_fixes | pass |
| TestRouteRunner_SharedOutbox_DepthCacheExercised | validates SharedOutbox route exercises depth cache QueryPending path (T19) | unit | low_fixes | pass |
| TestRouteRunner_DirectHold_NoQueryPending | validates DirectHold route never calls QueryPending (T19) | unit | low_fixes | pass |
| TestDrainerNameGeneration | validates drainer name generation produces correct numeric suffixes for all indices (T20) | unit | low_fixes | pass |
| TestOutboxDrainerConfig_DrainBatchSize_Default | validates DrainBatchSize=0 defaults to 100 (T21) | unit | low_fixes | pass |
| TestOutboxDrainerConfig_DrainBatchSize_Custom | validates explicit DrainBatchSize is respected (T21) | unit | low_fixes | pass |
| TestOutboxDrainer_FinalDrain_CompletesAfterCancel | validates finalDrain completes after Run context cancelled (T22) | unit | low_fixes | pass |
| TestWithGlobalMaxInFlight_NegativeClampedToZero | validates negative globalMaxInFlight clamped to 0 (T24) | unit | low_fixes | pass |
| TestWithGlobalMaxInFlight_Zero_DisablesThrottling | validates zero globalMaxInFlight disables throttling (T24) | unit | low_fixes | pass |
| TestWithGlobalMaxInFlight_Positive_Accepted | validates positive globalMaxInFlight accepted (T24) | unit | low_fixes | pass |
| TestRouter_Route_PayloadDeepCopy | validates Payload mutation in one handler does not affect another (T23) | unit | low_fixes | pass |
| TestRouter_Route_NilPayload | validates nil Payload does not panic and handlers receive nil (T23) | unit | low_fixes | pass |
| TestRouter_Route_EmptyPayload | validates empty Payload is deep-copied as non-nil zero-length slice (T23) | unit | low_fixes | pass |
| TestRouter_Route_OriginalPayloadUnmutated | validates original Publish.Payload not affected by handler mutations (T23) | unit | low_fixes | pass |
| TestRouter_Route_ConcurrentHandlers_IndependentPayloads | validates concurrent handlers receive independent payload copies (T23) | unit | low_fixes | pass |
| TestOutboxDrainerConfig_DrainBatchSize_NegativeClamped | validates negative DrainBatchSize clamped to default 100 (T21) | unit | low_fixes | pass |
| TestOutboxDrainerConfig_DrainMaxBatchSize_FloorsToBatchSize | validates DrainMaxBatchSize < DrainBatchSize is raised to match (T21) | unit | low_fixes | pass |
| TestRouteRunner_SharedOutbox_NilOutboxStore_Retries | validates SharedOutbox with nil OutboxStore retries without panic (T19) | unit | low_fixes | pass |
| TestRouter_Route_ConcurrentPropertiesRead | validates concurrent handlers reading shared Properties do not race (T23) | unit | low_fixes | pass |
| TestDirectHold_RetryUnsupported_FallsToDLQ | validates direct_hold fallback to DLQ when del.Retry returns ErrNotSupported (S6) | unit | retry_fallback | pass |
| TestDirectHold_RetryUnsupported_DLQAlsoFails_ReturnsError | validates error propagation when both retry and DLQ fail (S6) | unit | retry_fallback | pass |
| TestHandleProcessorError_RetryUnsupported_FallsToDLQ | validates processor error DLQ fallback when retry unsupported (S6) | unit | retry_fallback | pass |
| TestSharedOutbox_RetryUnsupported_FallsToDLQ | validates shared_outbox persist error DLQ fallback when retry unsupported (S6) | unit | retry_fallback | pass |
| TestHandleExpired_RetryUnsupported_FallsToDLQ | validates expired message DLQ fallback on failed first write and unsupported retry (S6) | unit | retry_fallback | pass |
| TestHandleResolveError_RetryUnsupported_FallsToDLQ | validates resolve error DLQ fallback when retry unsupported (S6) | unit | retry_fallback | pass |
| TestDirectHold_RetrySupported_NoFallback | validates normal retry path with no DLQ fallback regression (S6) | unit | retry_fallback | pass |
| TestSessionManager_ReconnectReconcileError_LogsAndPropagates | validates Reconcile error propagation from handleEvents on reconnect (S9) | unit | session_reconnect | pass |
| TestSessionManager_ReconnectReconcileError_EmitsMetric | validates MetricReconcileFailures emitted on reconnect Reconcile failure (S9) | unit | session_reconnect | pass |
| TestSessionManager_ReconnectReconcileOK_NoError | validates successful reconnect Reconcile does not emit failure metric (S9) | unit | session_reconnect | pass |
| TestSessionManager_RenewLoop_ReconnectReconcileError_Exits | validates renewLoop exits when Reconcile fails on reconnect (S9) | unit | session_reconnect | pass |
| TestRouteRunner_ProcessorPanic_DoesNotCrash | validates panicking processor does not crash process (S13) | unit | s13_delivery_panic | pass |
| TestRouteRunner_ProcessorPanic_RetriesDelivery | validates delivery retried after processor panic with reason (S13) | unit | s13_delivery_panic | pass |
| TestRouteRunner_SenderPanic_DoesNotCrash | validates panicking sender does not crash process (S13) | unit | s13_delivery_panic | pass |
| TestRouteRunner_SenderPanic_RetriesDelivery | validates delivery retried after sender panic (S13) | unit | s13_delivery_panic | pass |
| TestRouteRunner_DeliveryPanic_EmitsMetric | validates DeliveryPanics counter emitted with route_id tag (S13) | unit | s13_delivery_panic | pass |
| TestRouteRunner_DeliveryPanic_SlotsReleased | validates semaphore slot released after panic so next msg proceeds (S13) | unit | s13_delivery_panic | pass |
| TestRouteRunner_DeliveryPanic_OtherMessagesUnaffected | validates concurrent deliveries unaffected by one panic (S13) | unit | s13_delivery_panic | pass |
| TestRouteRunner_DeliveryPanic_RetryFails_NoSecondPanic | validates retry error after panic does not crash (S13) | unit | s13_delivery_panic | pass |
| TestRouteRunner_DeliveryPanic_RetryPanics_NoProcessCrash | validates nested panic in retry handler caught by inner recover (S13) | unit | s13_delivery_panic | pass |
| TestIntegration_HTTPPost_RuntimePipeline_FakeSender | validates full HTTP POST through runtime pipeline to fake sender | integration | http_transport | pass |
| TestIntegration_HTTPPost_FilterDrop_NoSend | validates filter processor drops spam messages without forwarding | integration | http_transport | pass |
| TestIntegration_SSEClient_ReceivesMultipleEvents | validates SSE client receives multiple broadcast events | integration | http_transport | pass |
| TestIntegration_HTTPPost_APIKeyAuth | validates API key authentication via X-API-Key and Bearer headers | integration | http_transport | pass |
| TestIntegration_HTTPPost_BodyTooLarge | validates body size limit enforcement returns 400 | integration | http_transport | pass |
| TestIntegration_HTTPPost_InvalidJSON | validates malformed JSON rejection returns 400 | integration | http_transport | pass |
| TestIntegration_HTTPPost_HeaderProcessing | verifies reserved headers stripped and correlation-id injected | integration | http_transport | pass |
| TestIntegration_HTTPPost_ReceiverNotReady | verifies POST before Run returns 503 Service Unavailable | integration | http_transport | pass |
| TestIntegration_Cluster_ForwardToBridge | validates HTTP POST forwarding between two bridges via HTTPForwarder | integration | http_cluster | pass |
| TestIntegration_Cluster_SSERedirect | validates SSE 307 redirect to remote bridge and event reception | integration | http_cluster | pass |
| TestIntegration_Cluster_ForwardLoopPrevention | verifies X-Bridge-Forwarded prevents infinite forwarding loops | integration | http_cluster | pass |
| TestIntegration_Cluster_ForwardToDeadPeer | verifies forwarding to unavailable peer returns 502 | integration | http_cluster | pass |
| TestIntegration_Cluster_ForwardPreservesEnvelope | validates envelope integrity through forward round-trip | integration | http_cluster | pass |
| TestIntegration_MQTT_To_SSE_CrossTransport | validates MQTT publish through bridge pipeline to SSE client | integration | cross_transport | pass |
| TestIntegration_HTTP_To_MQTT_CrossTransport | validates HTTP POST through bridge pipeline to MQTT publish | integration | cross_transport | pass |
<!-- longrunning: transport_combos (UC7-UC11) -->
| TestUC7_SQS_FIFO_Ordering_Through_MQTT | validates FIFO message ordering through MQTT bridge | longrunning | transport_combos | pass |
| TestUC8_MultiProtocol_FanOut | validates fan-out from SQS to 2 MQTT topics and 1 SQS queue | longrunning | transport_combos | pass |
| TestUC9_MQTT_QoS2_Stress | validates 5,000 QoS 2 messages with no duplicates | longrunning | transport_combos | pass |
| TestUC10_HTTP_Inject_To_MQTT | validates runtime Inject API delivers messages to MQTT | longrunning | transport_combos | pass |
| TestUC11_SQS_To_SQS_Direct | validates SQS-to-SQS direct via SharedOutbox without MQTT | longrunning | transport_combos | pass |
<!-- longrunning: cluster_lease (UC12-UC16) -->
| TestUC12_LeaseExpiry_Standby_Takeover | validates standby takes over when leader lease expires | longrunning | cluster_lease | planned |
| TestUC13_Lease_Renewal_Under_Load | validates lease renewal under sustained message load | longrunning | cluster_lease | planned |
| TestUC14_Cluster_Graceful_Shutdown | validates graceful shutdown with drain before lease release | longrunning | cluster_lease | planned |
| TestUC15_Cluster_Split_Brain_Fencing | validates fencing token prevents split-brain duplicate sends | longrunning | cluster_lease | planned |
| TestUC16_Cluster_Lease_Jitter | validates lease renewal jitter prevents thundering herd | longrunning | cluster_lease | planned |
<!-- longrunning: message_shape (UC17-UC21) -->
| TestUC17_LargePayloads_200KB | validates 200KB payloads round-trip with SHA256 integrity | longrunning | message_shape | pass |
| TestUC18_TinyPayloads_HighThroughput | validates 50,000 tiny payloads at high throughput | longrunning | message_shape | pass |
| TestUC19_MixedPayloadSizes | validates mixed tiny/medium/large payloads delivered correctly | longrunning | message_shape | pass |
| TestUC20_HeaderHeavy_50Headers | validates 50-header messages preserved through MQTT bridge | longrunning | message_shape | pass |
| TestUC21_BinaryPayload_RoundTrip | validates binary payloads with 0x00/0xFF round-trip via base64 | longrunning | message_shape | pass |
<!-- longrunning: routing_filtering (UC22-UC26) -->
| TestUC22_ContentBased_Routing | validates content-based routing to different target queues | longrunning | routing_filtering | planned |
| TestUC23_Header_Filter_Processor | validates header-based filter processor drops matching messages | longrunning | routing_filtering | planned |
| TestUC24_Dynamic_Destination_Override | validates processor-driven HeaderRouteOverride destination | longrunning | routing_filtering | planned |
| TestUC25_Multi_Processor_Chain | validates ordered execution of multiple processors | longrunning | routing_filtering | planned |
| TestUC26_Processor_Error_DLQ | validates permanent processor errors route to DLQ | longrunning | routing_filtering | planned |
<!-- longrunning: failure_recovery (UC27-UC31) -->
| TestUC27_Transient_Sender_Failure_Recovery | validates recovery after transient sender failures | longrunning | failure_recovery | planned |
| TestUC28_Outbox_Claim_Recovery | validates stale claim recovery after instance crash | longrunning | failure_recovery | planned |
| TestUC29_DLQ_Replay | validates DLQ replay reprocesses failed messages | longrunning | failure_recovery | planned |
| TestUC30_Expired_Message_DLQ | validates expired messages routed to DLQ | longrunning | failure_recovery | planned |
| TestUC31_Connection_Reconnect | validates message flow resumes after transport reconnect | longrunning | failure_recovery | planned |
<!-- longrunning: backpressure (UC32-UC37) -->
| TestUC32_MaxInFlight_Throttle | validates MaxInFlight limits concurrent processing | longrunning | backpressure | planned |
| TestUC33_Global_MaxInFlight | validates global semaphore across multiple routes | longrunning | backpressure | planned |
| TestUC34_Visibility_Extension | validates SQS visibility timeout extension under slow processing | longrunning | backpressure | planned |
| TestUC35_Burst_Absorption | validates burst traffic absorbed without message loss | longrunning | backpressure | planned |
| TestUC36_Drain_Batch_Adaptive_Sizing | validates drain batch size adapts to send success rate | longrunning | backpressure | planned |
| TestUC37_Concurrent_Routes_Isolation | validates concurrent routes do not interfere with each other | longrunning | backpressure | planned |
<!-- longrunning: outbox_modes (UC38-UC41) -->
| TestUC38_OutboxDepthLimit | validates MaxOutboxDepth=100 rejects messages when outbox is full | longrunning | outbox_modes | pass |
| TestUC39_AckAfterOutboxPersist | validates AckAfterOutboxPersist acks source before drain completes | longrunning | outbox_modes | pass |
| TestUC40_AdaptiveDrain_Backoff | validates AdaptiveBackoff reduces drain cycles during idle periods | longrunning | outbox_modes | pass |
| TestUC41_IdempotentOutbox_Persist | validates outbox deduplication prevents duplicates on SQS redelivery | longrunning | outbox_modes | pass |
<!-- longrunning: gap_tests -->
| TestGAP_BrokerHardCrash_SharedOutbox | validates SharedOutbox replay after broker hard kill with total state loss | longrunning | broker_resilience_gap | pending |
| TestGAP_BrokerDisconnect_KeepAliveDetection | validates KeepAlive detection timing and reconnection after broker disconnect | longrunning | broker_resilience_gap | pending |
| TestGAP_DynamoDBOutage_LeaseRenewal | validates lease stepdown and re-acquire during DynamoDB outage | longrunning | lease_resilience_gap | pending |
| TestGAP_CircuitBreakerProcessor_Lifecycle | validates circuit breaker closed-open-half_open-closed lifecycle | longrunning | processor_gap | pending |
| TestGAP_TransformProcessor_JSONPathMapping | validates JSON transform processor with JSONPath field mapping | longrunning | processor_gap | pending |
| TestGAP_GoroutineLeak_StartStopCycle | validates zero goroutine leaks across multiple start/stop cycles | longrunning | performance_gap | pending |
| TestGAP_HTTPFullSuite | validates all admin and monitor HTTP endpoints with auth | longrunning | httpapi_gap | pending |
| TestGAP_HTTPBridgeStartStop | validates bridge start/stop lifecycle via HTTP endpoints | longrunning | httpapi_gap | pending |
| TestGAP_HTTPDLQManagement | validates DLQ list replay purge via HTTP admin endpoints | longrunning | httpapi_gap | pending |
| TestGAP_ShutdownWithOutboxInFlight | validates finalDrain completes when Stop called during active draining | longrunning | shutdown_gap | pending |
| TestGAP_DoubleStopSafety | validates concurrent Stop calls do not panic or deadlock | longrunning | shutdown_gap | pending |
| TestGAP_FanOutSharedOutbox_ThreeTargets | validates SharedOutbox fan-out to 3 MQTT targets with complete delivery | longrunning | delivery_gap | pending |
| TestGAP_BackpressureFairness_MixedFastSlow | validates GlobalMaxInFlight fairness between fast and slow routes | longrunning | backpressure_gap | pending |
<!-- integration: ddb_config_overlay -->
| TestDDBOverlay_MergesSessionsFromDDB | validates DDB overlay session merges into base config | integration | ddb_config | pass |
| TestDDBOverlay_ReplacesSessionByID | validates DDB overlay replaces session by matching ID | integration | ddb_config | pass |
| TestDDBOverlay_AddsNewRoute | validates DDB overlay appends new route alongside base routes | integration | ddb_config | pass |
| TestDDBOverlay_ReplacesRouteByID | validates DDB overlay replaces route by matching ID | integration | ddb_config | pass |
| TestDDBOverlay_OverridesBridgeSettings | validates DDB overlay overrides non-zero bridge settings | integration | ddb_config | pass |
| TestDDBOverlay_ReplacesConfigWatch | validates DDB overlay replaces config_watch block | integration | ddb_config | pass |
| TestDDBOverlay_ReplacesStorePerRole | validates DDB overlay replaces stores per role independently | integration | ddb_config | pass |
| TestDDBOverlay_EmptyOverlay_PreservesBase | validates empty DDB overlay preserves all base values | integration | ddb_config | pass |
| TestDDBOverlay_PartialOverlay_OnlyAddsNewSenders | validates partial DDB overlay appends senders | integration | ddb_config | pass |
<!-- integration: ddb_config_watch -->
| TestDDBWatch_VersionChangeTriggersEmission | validates DDB version change triggers Manager emission | integration | ddb_config | pass |
| TestDDBWatch_NoVersionChange_NoEmission | validates no emission when DDB version unchanged | integration | ddb_config | pass |
| TestDDBWatch_ManagerRebuildsMergedConfig | validates Manager re-merges all layers on DDB change | integration | ddb_config | pass |
| TestDDBWatch_InvalidMergedConfig_DroppedByManager | validates Manager drops invalid merged configs | integration | ddb_config | pass |
| TestDDBWatch_ManagerStop_ClosesChannel | validates Manager.Stop closes watch channel | integration | ddb_config | pass |
| TestDDBWatch_ContextCancel_ClosesChannel | validates context cancel closes watch channel | integration | ddb_config | pass |
| TestDDBWatch_MultipleOverlayChanges_EachEmits | validates sequential DDB changes each emit merged config | integration | ddb_config | pass |
| TestDDBWatch_RapidSaves_AtLeastOneEmission | validates rapid DDB saves produce at least one emission | integration | ddb_config | pass |
| TestDDBWatch_InvalidThenValid_OnlyValidEmits | validates invalid overlay dropped then valid one emits | integration | ddb_config | pass |
<!-- integration: ddb_config_supervisor -->
| TestDDBSupervisor_InitialLoadAndRun | validates supervisor starts runtime from merged config | integration | ddb_config | pass |
| TestDDBSupervisor_OverlayChangeSwapsRuntime | validates DDB change triggers supervisor runtime swap | integration | ddb_config | pass |
| TestDDBSupervisor_SwapEvent_ReportsCorrectConfigs | validates SwapEvent has correct old and new configs | integration | ddb_config | pass |
| TestDDBSupervisor_SwapEvent_ReportsMode | validates SwapEvent reports correct swap mode | integration | ddb_config | pass |
| TestDDBSupervisor_DebouncedStrategy_CoalescesChanges | validates debounced strategy coalesces rapid DDB changes | integration | ddb_config | pass |
| TestDDBSupervisor_SequentialChanges_EachApplied | validates sequential DDB changes each trigger a swap | integration | ddb_config | pass |
| TestDDBSupervisor_PrepareCommitMode_WithExclusiveSession | validates auto-detect PrepareCommit for exclusive transport | integration | ddb_config | pass |
<!-- integration: ddb_config_rollback -->
| TestDDBRollback_InvalidOverlay_ManagerDrops | validates invalid overlay dropped, supervisor unchanged | integration | ddb_config | pass |
| TestDDBRollback_ValidOverlayButBuildFails_KeepsOldConfig | validates build failure triggers rollback to old config | integration | ddb_config | pass |
| TestDDBRollback_ValidOverlayButStartFails_RecoversOldConfig | validates start failure recovers old config | integration | ddb_config | pass |
| TestDDBRollback_MultipleFailures_OldConfigSurvives | validates old config survives multiple failure types | integration | ddb_config | pass |
| TestDDBRollback_InvalidThenValid_RecoversFully | validates recovery from invalid to valid overlay | integration | ddb_config | pass |
<!-- integration: ddb_config_transport -->
| TestDDBTransport_SQS_ConfigChangeSwapsQueue | validates SQS queue swap via DDB config change | integration | ddb_config | pass |
| TestDDBTransport_SQS_NewRouteAdded | validates new SQS route added via DDB overlay | integration | ddb_config | pass |
| TestDDBTransport_ConfigRemovesRoute | validates route removal when DDB overlay drops it | integration | ddb_config | pass |
<!-- integration: config_api_crud -->
| TestConfigAPI_GetConfig_ReturnsCurrentConfig | validates GET /config returns effective config over real HTTP | integration | config_api | pass |
| TestConfigAPI_CreateTransaction_Returns201WithTxnID | validates POST /transactions returns 201 with txn_id | integration | config_api | pass |
| TestConfigAPI_CreateTransaction_WithCustomTTL | validates custom TTL is respected in transaction expiry | integration | config_api | pass |
| TestConfigAPI_GetTransaction_ReturnsPatchCountAndPreview | validates GET /transactions/{txnID} returns state and preview | integration | config_api | pass |
| TestConfigAPI_PatchTransaction_ReturnsMergedPreview | validates PATCH returns merged preview with overlay applied | integration | config_api | pass |
| TestConfigAPI_CommitTransaction_Returns200 | validates commit succeeds after valid patch | integration | config_api | pass |
| TestConfigAPI_RollbackTransaction_Returns200 | validates rollback succeeds and returns expected status | integration | config_api | pass |
| TestConfigAPI_RollbackThenNewTransaction_Succeeds | validates new transaction allowed after rollback | integration | config_api | pass |
| TestConfigAPI_CommitThenNewTransaction_Succeeds | validates new transaction allowed after commit | integration | config_api | pass |
<!-- integration: config_api_auth -->
| TestConfigAPI_Auth_NoKey_Returns401 | validates all config endpoints reject requests without API key | integration | config_api | pass |
| TestConfigAPI_Auth_WrongKey_Returns401 | validates all config endpoints reject incorrect API key | integration | config_api | pass |
| TestConfigAPI_Auth_ValidXAPIKey_Succeeds | validates X-API-Key header authentication | integration | config_api | pass |
| TestConfigAPI_Auth_ValidBearerToken_Succeeds | validates Bearer token authentication | integration | config_api | pass |
| TestConfigAPI_Auth_CorrelationHeaders_Returned | validates correlation headers in responses | integration | config_api | pass |
| TestConfigAPI_Auth_CustomCorrelationID_Echoed | validates client correlation ID echoed back | integration | config_api | pass |
| TestConfigAPI_SecurityHeaders_Present | validates security headers on config API responses | integration | config_api | pass |
<!-- integration: config_api_validation -->
| TestConfigAPI_Patch_InvalidRouteRef_Returns422 | validates bad receiver_id returns 422 with validation_errors | integration | config_api | pass |
| TestConfigAPI_Patch_MissingBindingRef_Returns422 | validates missing binding reference returns 422 | integration | config_api | pass |
| TestConfigAPI_Patch_InvalidDeliveryMode_Returns422 | validates invalid delivery_mode returns 422 | integration | config_api | pass |
| TestConfigAPI_Patch_InvalidJSON_Returns400 | validates non-JSON body produces 400 | integration | config_api | pass |
| TestConfigAPI_Patch_AfterInvalidPatch_TransactionRemains | validates transaction not poisoned by rejected patch | integration | config_api | pass |
<!-- integration: config_api_disk -->
| TestConfigAPI_Commit_WritesConfigToDisk | validates commit writes merged YAML to config file | integration | config_api | pass |
| TestConfigAPI_Commit_AtomicWrite_NoPartialFiles | validates no temp files remain after commit | integration | config_api | pass |
| TestConfigAPI_Commit_PreservesFilePermissions | validates file permissions preserved after commit | integration | config_api | pass |
| TestConfigAPI_Commit_MultiplePatchesMergedOnDisk | validates multiple patches reflected on disk | integration | config_api | pass |
| TestConfigAPI_Rollback_DiskUnchanged | validates rollback leaves config file unchanged | integration | config_api | pass |
| TestConfigAPI_Commit_ConfigRoundTrip | validates committed config round-trips through disk | integration | config_api | pass |
<!-- integration: config_api_watcher -->
| TestConfigAPI_Pipeline_CommitAddsRoute_SupervisorSwaps | validates commit adds route, supervisor swaps runtime | integration | config_api | pass |
| TestConfigAPI_Pipeline_CommitChangesLogLevel | validates log_level change triggers supervisor swap | integration | config_api | pass |
| TestConfigAPI_Pipeline_RollbackDoesNotTriggerSwap | validates rollback does not trigger supervisor swap | integration | config_api | pass |
| TestConfigAPI_Pipeline_SequentialCommits_EachApplied | validates sequential commits each trigger swaps | integration | config_api | pass |
<!-- integration: config_api_edge -->
| TestConfigAPI_TransactionIsolation_OnlyOneActive | validates second transaction returns 409 Conflict | integration | config_api | pass |
| TestConfigAPI_TransactionIsolation_WrongTxnID_Returns404 | validates wrong txn ID returns 404 | integration | config_api | pass |
| TestConfigAPI_TransactionIsolation_ExpiredTxn_Returns404 | validates expired transaction returns 404 | integration | config_api | pass |
| TestConfigAPI_TransactionIsolation_AfterExpiry_NewTxnAllowed | validates new transaction allowed after expiry | integration | config_api | pass |
| TestConfigAPI_MultiPatch_Accumulation | validates three sequential patches accumulate correctly | integration | config_api | pass |
| TestConfigAPI_MultiPatch_LastPatchWins | validates last patch wins for same field | integration | config_api | pass |
| TestConfigAPI_Redaction_GetConfig_HidesAPIKeys | validates API keys redacted in GET /config | integration | config_api | pass |
| TestConfigAPI_Redaction_PatchPreview_HidesAPIKeys | validates API keys redacted in PATCH preview | integration | config_api | pass |
| TestConfigAPI_TTL_MaxClampedTo30m | validates TTL exceeding max is clamped to 30m | integration | config_api | pass |
| TestIntegration_Edge_EmptyPayload | validates send/receive with nil and zero-length payloads | integration | amqp10_edge | pass |
| TestIntegration_Edge_LargePayload | validates 1 MB payload integrity via SHA-256 checksum | integration | amqp10_edge | pass |
| TestIntegration_Edge_SendContextTimeout | validates send with cancelled context returns error | integration | amqp10_edge | pass |
| TestIntegration_Edge_ReceiveContextCancel | validates clean receiver shutdown on context cancel | integration | amqp10_edge | pass |
| TestIntegration_Edge_DoubleAck | validates idempotent double ack via sync.Once | integration | amqp10_edge | pass |
| TestIntegration_Edge_DoubleRetry | validates idempotent double retry via sync.Once | integration | amqp10_edge | pass |
| TestIntegration_Edge_AckThenRetry | validates only first settlement takes effect | integration | amqp10_edge | pass |
| TestIntegration_Edge_SendAfterSessionClose | validates send on closed session returns error | integration | amqp10_edge | pass |
| TestIntegration_Edge_ReceiverOnClosedSession | validates receiver on closed session returns error | integration | amqp10_edge | pass |
| TestIntegration_Edge_WrongCredentials | validates auth error with invalid credentials | integration | amqp10_edge | pass |
| TestIntegration_Edge_SessionHealthTransitions | validates Health at before-start, after-start, after-close | integration | amqp10_edge | pass |
| TestIntegration_Edge_MulticastRouting | validates fan-out delivery with RoutingMulticast | integration | amqp10_edge | pass |
| TestIntegration_Edge_SendBatchPartialVerify | validates all batch messages received with correct IDs | integration | amqp10_edge | pass |
| TestIntegration_Edge_HeaderUnicodeAndLongValues | validates unicode and long header value round-trip | integration | amqp10_edge | pass |
| TestIntegration_Edge_EnvelopeFieldsRoundTrip | validates all envelope fields survive send/receive | integration | amqp10_edge | pass |
| TestIntegration_Edge_EmptyPayload | validates send/receive with nil and zero-length payloads | integration | amqp091_edge | pass |
| TestIntegration_Edge_LargePayload | validates 1 MB payload integrity via SHA-256 checksum | integration | amqp091_edge | pass |
| TestIntegration_Edge_SendContextTimeout | validates send with cancelled context returns error | integration | amqp091_edge | pass |
| TestIntegration_Edge_ReceiveContextCancel | validates clean receiver shutdown on context cancel | integration | amqp091_edge | pass |
| TestIntegration_Edge_DoubleAck | validates idempotent double ack via sync.Once | integration | amqp091_edge | pass |
| TestIntegration_Edge_DoubleRetry | validates idempotent double retry via sync.Once | integration | amqp091_edge | pass |
| TestIntegration_Edge_AckThenRetry | validates only first settlement takes effect | integration | amqp091_edge | pass |
| TestIntegration_Edge_SendAfterSessionClose | validates send on closed session returns error | integration | amqp091_edge | pass |
| TestIntegration_Edge_ReceiverOnClosedSession | validates receiver on closed session returns error | integration | amqp091_edge | pass |
| TestIntegration_Edge_WrongCredentials | validates auth error with invalid credentials | integration | amqp091_edge | pass |
| TestIntegration_Edge_SessionHealthTransitions | validates Health at before-start, after-start, after-close | integration | amqp091_edge | pass |
| TestIntegration_Edge_ExchangeRouting | validates end-to-end message flow through exchange/binding | integration | amqp091_edge | pass |
| TestIntegration_Edge_ReconcilePlan | validates Reconcile stores subscriptions and Health reflects count | integration | amqp091_edge | pass |
| TestIntegration_Edge_SendBatchAllReceived | validates all batch messages received with correct IDs | integration | amqp091_edge | pass |
| TestIntegration_Edge_HeaderRoundTrip | validates unicode and long header value round-trip | integration | amqp091_edge | pass |
| TestIntegration_Edge_PrefetchHonored | validates PrefetchCount=1 limits in-flight to 1 | integration | amqp091_edge | pass |
| TestUC90_SQS_To_RabbitMQ_SharedOutbox | validates end-to-end 1000 msg pipeline SQS -> RabbitMQ (SharedOutbox) | longrunning | amqp091_pipeline | pass |
| TestUC91_SQS_To_Artemis_SharedOutbox | validates end-to-end 1000 msg pipeline SQS -> Artemis (SharedOutbox) | longrunning | amqp10_pipeline | pass |
| TestUC92_RabbitMQ_BrokerKillRestart | validates SharedOutbox recovery after RabbitMQ broker kill/restart | longrunning | amqp091_resilience | pass |
| TestUC93_Artemis_BrokerKillRestart | validates SharedOutbox recovery after Artemis broker kill/restart | longrunning | amqp10_resilience | skip |
| TestUC94_AMQP091_HighThroughput | validates 5000 msg throughput through RabbitMQ pipeline | longrunning | amqp091_throughput | pass |
| TestUC95_AMQP10_HighThroughput | validates 5000 msg throughput through Artemis pipeline | longrunning | amqp10_throughput | pass |
| TestUC96_CrossProtocol_RabbitMQ_Artemis | validates cross-protocol message flow RabbitMQ + Artemis | longrunning | amqp_cross_protocol | pass |
| TestUC97_AMQP091_MultiConsumer_CompetingConsumers | validates 3 competing consumers on same RabbitMQ queue | longrunning | amqp091_multi_consumer | pass |
| TestIntegration_SQS_Delivery_AckRemovesMessage | validates Ack deletes message from SQS queue | integration | sqs_delivery | pass |
| TestIntegration_SQS_Delivery_RetryMakesMessageReappear | validates Retry with zero delay causes redelivery | integration | sqs_delivery | pass |
| TestIntegration_SQS_Delivery_ExtendPreventsRedelivery | validates Extend pushes visibility timeout forward | integration | sqs_delivery | pass |
| TestIntegration_SQS_HeaderRoundTrip | validates custom headers survive send→receive round-trip | integration | sqs_delivery | pass |
| TestIntegration_SQS_AutoExtendKeepsMessageInvisible | validates auto-extend prevents redelivery during long processing | integration | sqs_delivery | pass |
| TestIntegration_OutboxDrainer_FullLifecycle | validates persist→claim→send→complete with real DynamoDB | integration | outbox_drainer | pass |
| TestIntegration_OutboxDrainer_StaleFencingToken | validates stale token rejected by DynamoDB conditional writes | integration | outbox_drainer | pass |
| TestIntegration_OutboxDrainer_ExpiredRecordRoutesDLQ | validates expired records route to DLQ and complete | integration | outbox_drainer | pass |
| TestIntegration_OutboxDrainer_PoisonMessageRoutesDLQ | validates poison messages route to DLQ and complete | integration | outbox_drainer | pass |
| TestIntegration_OutboxDrainer_ConcurrentDrainers | validates concurrent drainers do not produce duplicate sends | integration | outbox_drainer | pass |
| TestIntegration_OutboxDrainer_AdaptiveBatchSize | validates batch size scales with throughput | integration | outbox_drainer | pass |
| TestIntegration_DLQRouter_RouteStoresEntry | validates Route creates DLQ entry in DynamoDB with all fields | integration | dlq_router | pass |
| TestIntegration_DLQRouter_AsyncBufferDrains | validates async buffer entries drain to DynamoDB via workers | integration | dlq_router | pass |
| TestIntegration_DLQRouter_ErrorClassification | validates error category and code persisted correctly | integration | dlq_router | pass |
| TestIntegration_DLQRouter_CloseDrainsBuffer | validates Close drains remaining buffer entries | integration | dlq_router | pass |
| TestIntegration_DLQRouter_ConcurrentRoutes | validates concurrent Route calls all persist safely | integration | dlq_router | pass |
| TestIntegration_SQS_Sender_QueueNameResolution | validates Sender resolves QueueName to URL on first Send | integration | sqs_queuename | pass |
| TestIntegration_SQS_Receiver_QueueNameResolution | validates Receiver resolves QueueName to URL on Run | integration | sqs_queuename | pass |
| TestIntegration_SQS_SenderReceiver_FullRoundTrip | validates Sender→SQS→Receiver round-trip with Subject and headers | integration | sqs_queuename | pass |
| TestIntegration_SQS_Sender_BatchThenReceive | validates batch-sent messages all received by Receiver | integration | sqs_queuename | pass |
| TestIntegration_OutboxDrainer_RealSQSSender_FullCycle | validates persist→drain→real SQS send→verify in queue | integration | outbox_drainer_sqs | pass |
| TestIntegration_OutboxDrainer_RealSQSSender_ExpiredToDLQ | validates expired record skips SQS, lands in DLQ | integration | outbox_drainer_sqs | pass |
| TestIntegration_OutboxDrainer_RealSQSSender_HeaderPreservation | validates DispatchHeaders survive outbox→SQS pipeline | integration | outbox_drainer_sqs | pass |
| TestGap_AMQP091_To_SQS_CrossTransport | validates AMQP 0-9-1→bridge→SQS cross-transport routing | longrunning | amqp_cross_transport | pass |
| TestGap_AMQP091_To_MQTT_CrossTransport | validates AMQP 0-9-1→bridge→MQTT cross-transport routing | longrunning | amqp_cross_transport | pass |
| TestDeliveryHook_DirectHold_Success | validates OnAttempt ingress+egress and OnSettled on successful direct hold delivery | unit | delivery_hook | pass |
| TestDeliveryHook_DirectHold_PermanentFailure_DLQ | validates OnSettled carries error when permanent send failure routes to DLQ | unit | delivery_hook | pass |
| TestDeliveryHook_DirectHold_NoHook_NoopSafe | validates delivery works when no hook is registered (noop default) | unit | delivery_hook | pass |
| TestDeliveryHook_DirectHold_TransientRetry_NoSettled | validates transient failure fires OnAttempt but not OnSettled when retry succeeds | unit | delivery_hook | pass |
| TestDeliveryHook_DirectHold_AttemptCarriesReceiveCount | validates Attempt field uses receiveCount+1 from envelope headers | unit | delivery_hook | pass |
| TestDeliveryHook_DirectHold_MaxAttemptFromPolicy | validates MaxAttempts field matches policy MaxReplayAttempts | unit | delivery_hook | pass |
| TestDeliveryHook_DirectHold_Drop_NoDLQ_RetryUnsupported | validates OnSettled fires with error when message is dropped | unit | delivery_hook | pass |
| TestDeliveryHook_DirectHold_ExpiredMessage_NoEgressHook | validates expired message fires ingress OnAttempt but no egress hooks | unit | delivery_hook | pass |
| TestDeliveryHook_DirectHold_SettledCarriesBindingID | validates OnSettled BindingID matches the dispatch plan binding | unit | delivery_hook | pass |
| TestDeliveryHook_DirectHold_ConcurrentDeliveries | validates concurrent deliveries each get independent hook calls | unit | delivery_hook | pass |
| TestDeliveryHook_SharedOutbox_Success | validates OnAttempt+OnSettled on successful outbox drain | unit | delivery_hook | pass |
| TestDeliveryHook_SharedOutbox_Poison | validates OnSettled fires with poison error when replay count exceeded | unit | delivery_hook | pass |
| TestDeliveryHook_SharedOutbox_Expired | validates OnSettled fires with ErrMessageExpired for expired records | unit | delivery_hook | pass |
| TestDeliveryHook_SharedOutbox_PermanentSendError | validates OnSettled fires when permanent send error routes to DLQ | unit | delivery_hook | pass |
| TestDeliveryHook_SharedOutbox_TransientNoSettled | validates transient send failure fires OnAttempt but not OnSettled | unit | delivery_hook | pass |
| TestDeliveryHook_SharedOutbox_AttemptIsReplayCountPlusOne | validates Attempt field is ReplayCount+1 in drainer hooks | unit | delivery_hook | pass |
| TestDeliveryHook_SharedOutbox_MultipleBatchRecords | validates each record in a drain batch fires independent hook calls | unit | delivery_hook | pass |
| TestDeliveryHook_Builder_RegisterPropagates | validates RegisterDeliveryHook on builder propagates to runtime | unit | delivery_hook | pass |
<!-- Task 14: exact-accounting chaos and release proofs -->
| TestAccountant_ReconcileClassifiesObservations | validates exact producer reconciliation categories and identity collisions | unit | task14_accounting | pass |
| TestAccountant_ReconcileOrdered | validates deterministic reorder reporting | unit | task14_accounting | pass |
| TestAccountant_ConcurrentObservations | validates concurrency-safe producer observations | unit | task14_accounting | pass |
| TestAccountant_NewRejectsInvalidExpectedKeys | validates producer accountant expected-key invariants | unit | task14_accounting | pass |
| TestIntegration_PersistentSessionQueuedQoS1AndQoS2 | validates same-client persistent-session queued delivery and exact accounting | integration | task14_mqtt | pass |
| TestReconnect_ReconcileTimeoutDegradesAndRecovers | validates bounded reconnect reconcile failure and recovery | unit | task14_mqtt | pass |
| TestRes_BrokerOutage_ReconnectResubscribesAndDelivers | validates real broker Stop/Restart reconnect, re-subscribe, and resumed delivery | integration | task14_mqtt | pass |
| TestConnectionFailure_BeforeCONNACK | validates bounded failure before MQTT CONNACK | integration | task14_mqtt | pass |
| TestConnectionFailure_AfterCONNACK | validates bounded failure after MQTT CONNACK | integration | task14_mqtt | pass |
| TestConnectionFailure_TCPAndDNS | validates bounded TCP and DNS connection failures | integration | task14_mqtt | pass |
| TestConnectionFailure_TLSTrustAndHandshake | validates bounded TLS trust and handshake failures | integration | task14_mqtt | pass |
| TestConnectionFailure_CredentialsNotAuthorized | validates MQTT not-authorized CONNACK handling | integration | task14_mqtt | pass |
| TestMQTTSettlementRecovery | validates bounded persistent-MQTT settlement recycle during DLQ outage with exact accounting | integration | task14_mqtt | pass |
| TestTask14_StoreOutageMatrix | validates independent/combined store outages and documented lease replacement recovery | longrunning | task14_chaos | pass |
| TestTask14_DuplicateClientIDTakeoverStorm | validates bounded duplicate-client takeover recovery | longrunning | task14_chaos | pass |
| TestTask14_ProcessKillChild | subprocess-only helper providing controlled source-ack, persisted-outbox, and ambiguous-send checkpoints | longrunning | task14_chaos | skip |
| TestTask14_ProcessKillBoundaries | validates SIGKILL recovery at source-ack, persisted-outbox, and ambiguous-send boundaries | longrunning | task14_chaos | pass |
| TestMQTTIngressMemory | validates maximum payload retention via the shared mandatory 512 MiB CI/release cgroup target | longrunning | task14_chaos | pass |
| TestTask14_EqualValuedMQTTIdentityGate | validates 100,000 equal-valued MQTT publishes remain distinct bridge events | longrunning | task14_identity | pass |
| TestMQTTEqualPublishIdentity | validates explicit producer-ID redelivery deduplication and preserved envelope identity | integration | task14_identity | pass |
| TestUC46_BrokerMessageSizeLimit | validates exact delivered and DLQ sets at the broker message-size limit | longrunning | task14_accounting | pass |
| TestReleaseWorkflow_FinalCommandTestsGatePublication | validates final cmd release publication and image paths depend on uncached aggregate gates | unit | task14_release | pass |
| TestWorkflows_BoundedMQTTIngressMemoryUsesSharedTarget | validates CI and final release invoke one mandatory bounded-memory target | unit | task14_release | pass |
