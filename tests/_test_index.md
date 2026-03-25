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
| TestPasswordCredential_Fields | validates PasswordCredential field assignment | unit | domain | pass |
| TestTLSMaterial_Fields | validates TLSMaterial field assignment | unit | domain | pass |
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
