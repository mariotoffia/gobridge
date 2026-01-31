---
name: implement-stateless-idempotency
description: "Step-by-step guide for implementing stateless services and idempotent operations following  patterns."
tools: ["Read", "Write", "Edit", "Bash", "Glob", "Grep"]
context:
  - type: file
    path: "architecture/**/*.md"
  - type: file
    path: "patterns/**/*.md"
---

# Skill: Implement Stateless and Idempotent Services

This skill teaches you how to implement stateless services and idempotent operations following  architectural patterns. You'll learn to build services that can scale horizontally, handle retries safely, and maintain data consistency in distributed systems.

Stateless services and idempotent operations are fundamental requirements for cloud-native applications. They enable horizontal scaling (any instance can handle any request), fault tolerance (retries are safe), and simplified deployment (no session affinity needed). In serverless environments, statelessness is enforced by design.

Idempotency ensures that calling an operation multiple times with the same input produces the same result. This is critical when working with at-least-once delivery systems like message queues, event buses, or any scenario where network failures may cause retries.

## Prerequisites

- Understanding of distributed systems concepts
- Familiarity with Clean Architecture principles
- Understanding of event-driven architectures

## Overview

In this skill, you will:
1. Understand idempotency requirements and patterns
2. Implement idempotency key storage
3. Create idempotency middleware for handlers
4. Design upsert operations for natural idempotency
5. Handle concurrent requests with conditional writes
6. Implement event deduplication
7. Test idempotent operations

## Step 1: Understand Idempotency Requirements

Before implementing, understand when and why idempotency is needed. Idempotency protects against duplicate processing that can occur from:
- Network retries (client didn't receive response, retries)
- Message redelivery (at-least-once delivery)
- Event duplication (event bus may deliver same event twice)
- User double-clicks (frontend form submissions)

### Idempotency Types and Records

```pseudocode
// core/domain/idempotency/types

// IdempotencyKey uniquely identifies an operation.
// Use event ID, request ID, or natural business key.
TYPE IdempotencyKey = String

// IdempotencyRecord stores the result of a processed operation.
TYPE IdempotencyRecord
    key: IdempotencyKey
    status: ProcessStatus
    result: Bytes           // Serialized result (optional)
    error: String           // Error message (optional)
    processedAt: Timestamp
    expiresAt: Timestamp

// ProcessStatus indicates the state of an idempotent operation.
TYPE ProcessStatus = String

CONSTANT StatusInProgress: ProcessStatus = "IN_PROGRESS"
CONSTANT StatusCompleted: ProcessStatus = "COMPLETED"
CONSTANT StatusFailed: ProcessStatus = "FAILED"

// IsTerminal returns true if the status is final.
METHOD ProcessStatus.IsTerminal() RETURNS Boolean
    RETURN this == StatusCompleted OR this == StatusFailed
END METHOD
```

The idempotency pattern follows this flow:
1. Receive request with idempotency key
2. Check if key exists in store
3. If found and completed: return cached result
4. If found and in-progress: wait or reject
5. If not found: mark as in-progress, process, store result

### Domain Errors

```pseudocode
// core/domain/idempotency/errors

// ErrDuplicateRequest indicates the operation was already processed.
CONSTANT ErrDuplicateRequest = Error("duplicate request: operation already processed")

// ErrOperationInProgress indicates the operation is currently being processed.
CONSTANT ErrOperationInProgress = Error("operation in progress by another request")

// ErrIdempotencyKeyRequired indicates missing idempotency key.
CONSTANT ErrIdempotencyKeyRequired = Error("idempotency key is required")

// ErrIdempotencyKeyExpired indicates the cached result has expired.
CONSTANT ErrIdempotencyKeyExpired = Error("idempotency record has expired")
```

## Step 2: Implement Idempotency Key Storage

Create a store for idempotency records with TTL for automatic cleanup.

### Repository Port

```pseudocode
// core/application/ports/outports/idempotency_store

// IdempotencyStore manages idempotency records.
INTERFACE IdempotencyStore
    // TryAcquire attempts to acquire a lock for the given key.
    // Returns nil if acquired, ErrOperationInProgress if already locked,
    // or the existing record if already processed.
    TryAcquire(ctx: Context, key: IdempotencyKey, ttl: Duration) RETURNS Result<IdempotencyRecord?, Error>

    // Complete marks the operation as completed with the result.
    Complete(ctx: Context, key: IdempotencyKey, result: Bytes) RETURNS Result<Void, Error>

    // Fail marks the operation as failed with an error message.
    Fail(ctx: Context, key: IdempotencyKey, errMsg: String) RETURNS Result<Void, Error>

    // Get retrieves an idempotency record by key.
    Get(ctx: Context, key: IdempotencyKey) RETURNS Result<IdempotencyRecord?, Error>
END INTERFACE
```

### Database Implementation

```pseudocode
// adapters/secondary/database/idempotency_store

// IdempotencyStore implements outports.IdempotencyStore with database storage.
TYPE IdempotencyStoreAdapter
    client: DatabaseClient
    tableName: String

// NewIdempotencyStore creates a new idempotency store.
CONSTRUCTOR NewIdempotencyStore(client: DatabaseClient, tableName: String) RETURNS IdempotencyStoreAdapter
    RETURN IdempotencyStoreAdapter{
        client: client,
        tableName: tableName
    }
END CONSTRUCTOR

// idempotencyItem is the database representation.
TYPE idempotencyItem
    pk: String          // Primary key
    status: String
    result: Bytes
    error: String
    processedAt: Integer    // Unix timestamp
    ttl: Integer            // Unix timestamp for auto-cleanup

// TryAcquire attempts to acquire a lock for idempotent processing.
METHOD IdempotencyStoreAdapter.TryAcquire(ctx: Context, key: IdempotencyKey, ttl: Duration) RETURNS Result<IdempotencyRecord?, Error>
    now = Now()
    expiresAt = now.Add(ttl)

    item = idempotencyItem{
        pk: key,
        status: StatusInProgress,
        processedAt: now.Unix(),
        ttl: expiresAt.Unix()
    }

    // Conditional put: only succeed if key doesn't exist
    // This provides atomic lock acquisition
    result = this.client.PutItemConditional(ctx, this.tableName, item, "pk NOT EXISTS")

    IF result.IsError() THEN
        // Check if condition failed (key already exists)
        IF result.Error().IsConditionFailed() THEN
            // Key exists - fetch existing record
            existing = this.Get(ctx, key)
            IF existing.IsError() THEN
                RETURN Error("failed to get existing record: " + existing.Error())
            END IF
            RETURN Ok(existing.Value())
        END IF
        RETURN Error("failed to put item: " + result.Error())
    END IF

    // Lock acquired successfully
    RETURN Ok(NULL)
END METHOD

// Complete marks the operation as successfully completed.
METHOD IdempotencyStoreAdapter.Complete(ctx: Context, key: IdempotencyKey, result: Bytes) RETURNS Result<Void, Error>
    update = Map{
        "status": StatusCompleted,
        "result": result
    }

    updateResult = this.client.UpdateItem(ctx, this.tableName, key, update)
    IF updateResult.IsError() THEN
        RETURN Error("failed to update item: " + updateResult.Error())
    END IF

    RETURN Ok()
END METHOD

// Fail marks the operation as failed.
METHOD IdempotencyStoreAdapter.Fail(ctx: Context, key: IdempotencyKey, errMsg: String) RETURNS Result<Void, Error>
    update = Map{
        "status": StatusFailed,
        "error": errMsg
    }

    updateResult = this.client.UpdateItem(ctx, this.tableName, key, update)
    IF updateResult.IsError() THEN
        RETURN Error("failed to update item: " + updateResult.Error())
    END IF

    RETURN Ok()
END METHOD

// Get retrieves an idempotency record by key.
METHOD IdempotencyStoreAdapter.Get(ctx: Context, key: IdempotencyKey) RETURNS Result<IdempotencyRecord?, Error>
    result = this.client.GetItem(ctx, this.tableName, key)
    IF result.IsError() THEN
        RETURN Error("failed to get item: " + result.Error())
    END IF

    IF result.Value() == NULL THEN
        RETURN Ok(NULL)
    END IF

    item = result.Value()
    RETURN Ok(IdempotencyRecord{
        key: key,
        status: ProcessStatus(item.status),
        result: item.result,
        error: item.error,
        processedAt: TimestampFromUnix(item.processedAt),
        expiresAt: TimestampFromUnix(item.ttl)
    })
END METHOD
```

## Step 3: Create Idempotency Middleware

Implement middleware that wraps handlers with idempotency logic.

```pseudocode
// core/application/middleware/idempotency

// IdempotentHandler wraps a handler function with idempotency logic.
TYPE IdempotentHandler<Request, Response>
    store: IdempotencyStore
    ttl: Duration
    keyFunc: Function(Request) RETURNS IdempotencyKey
    handler: Function(Context, Request) RETURNS Result<Response, Error>

// NewIdempotentHandler creates a new idempotent handler wrapper.
CONSTRUCTOR NewIdempotentHandler<Request, Response>(
    store: IdempotencyStore,
    ttl: Duration,
    keyFunc: Function(Request) RETURNS IdempotencyKey,
    handler: Function(Context, Request) RETURNS Result<Response, Error>
) RETURNS IdempotentHandler<Request, Response>
    RETURN IdempotentHandler<Request, Response>{
        store: store,
        ttl: ttl,
        keyFunc: keyFunc,
        handler: handler
    }
END CONSTRUCTOR

// Handle executes the handler with idempotency guarantees.
METHOD IdempotentHandler<Request, Response>.Handle(ctx: Context, req: Request) RETURNS Result<Response, Error>
    // Extract idempotency key from request
    key = this.keyFunc(req)
    IF key == "" THEN
        RETURN Error(ErrIdempotencyKeyRequired)
    END IF

    // Try to acquire lock
    existingResult = this.store.TryAcquire(ctx, key, this.ttl)
    IF existingResult.IsError() THEN
        RETURN Error("failed to acquire idempotency lock: " + existingResult.Error())
    END IF
    existing = existingResult.Value()

    // If existing record found, handle based on status
    IF existing != NULL THEN
        SWITCH existing.status
            CASE StatusCompleted:
                // Return cached result
                result = Deserialize<Response>(existing.result)
                IF result.IsError() THEN
                    RETURN Error("failed to deserialize cached result: " + result.Error())
                END IF
                RETURN Ok(result.Value())

            CASE StatusFailed:
                // Return cached error
                RETURN Error(ErrDuplicateRequest + ": " + existing.error)

            CASE StatusInProgress:
                // Another request is processing
                RETURN Error(ErrOperationInProgress)
        END SWITCH
    END IF

    // Process the request
    result = this.handler(ctx, req)
    IF result.IsError() THEN
        // Mark as failed
        this.store.Fail(ctx, key, result.Error().Message())
        RETURN result.Error()
    END IF

    // Serialize and store result
    resultBytes = Serialize(result.Value())
    IF resultBytes.IsError() THEN
        RETURN Error("failed to serialize result: " + resultBytes.Error())
    END IF

    storeResult = this.store.Complete(ctx, key, resultBytes.Value())
    IF storeResult.IsError() THEN
        // Log but don't fail - operation succeeded
        // Next retry will find IN_PROGRESS or re-process
        Log("warning: failed to complete idempotency record: " + storeResult.Error())
    END IF

    RETURN result
END METHOD
```

### Using the Middleware

```pseudocode
// adapters/primary/lambda/handler

// CreateOrderRequest is the incoming request.
TYPE CreateOrderRequest
    idempotencyKey: String
    customerID: String
    productID: String
    quantity: Integer

// CreateOrderResponse is the operation result.
TYPE CreateOrderResponse
    orderID: String
    status: String

// OrderHandler handles order creation with idempotency.
TYPE OrderHandler
    idempotentHandler: IdempotentHandler<CreateOrderRequest, CreateOrderResponse>

// NewOrderHandler creates a new order handler.
CONSTRUCTOR NewOrderHandler(store: IdempotencyStore, orderService: OrderService) RETURNS OrderHandler
    // Key function extracts idempotency key from request
    keyFunc = FUNCTION(req: CreateOrderRequest) RETURNS IdempotencyKey
        RETURN IdempotencyKey(req.idempotencyKey)
    END FUNCTION

    // Wrap the actual handler with idempotency logic
    idempotentHandler = NewIdempotentHandler(
        store,
        Duration(24 * Hour),  // TTL for idempotency records
        keyFunc,
        FUNCTION(ctx: Context, req: CreateOrderRequest) RETURNS Result<CreateOrderResponse, Error>
            RETURN orderService.CreateOrder(ctx, req)
        END FUNCTION
    )

    RETURN OrderHandler{idempotentHandler: idempotentHandler}
END CONSTRUCTOR

// Handle processes the order request idempotently.
METHOD OrderHandler.Handle(ctx: Context, req: CreateOrderRequest) RETURNS Result<CreateOrderResponse, Error>
    RETURN this.idempotentHandler.Handle(ctx, req)
END METHOD
```

## Step 4: Design Upsert Operations

Upsert operations provide natural idempotency through "create or update" semantics.

```pseudocode
// adapters/secondary/database/asset_repository

// AssetRepository implements asset persistence with idempotent operations.
TYPE AssetRepository
    client: DatabaseClient
    tableName: String

// assetItem is the database representation.
TYPE assetItem
    pk: String
    sk: String
    assetID: String
    facilityID: String
    assetType: String
    capacityKW: Float
    currentLoad: Float
    state: String
    updatedAt: Timestamp

// UpsertAsset creates or updates an asset idempotently.
// Calling this multiple times with the same asset produces the same result.
METHOD AssetRepository.UpsertAsset(ctx: Context, a: Asset) RETURNS Result<Void, Error>
    item = assetItem{
        pk: "ASSET#" + a.ID,
        sk: "METADATA",
        assetID: a.ID,
        facilityID: a.FacilityID,
        assetType: a.Type,
        capacityKW: a.Capacity,
        currentLoad: a.CurrentLoad,
        state: a.State,
        updatedAt: Now()
    }

    // PutItem is idempotent: same key, same data = same result
    // Multiple calls produce identical outcome
    result = this.client.PutItem(ctx, this.tableName, item)
    IF result.IsError() THEN
        RETURN Error("failed to put asset: " + result.Error())
    END IF

    RETURN Ok()
END METHOD
```

## Step 5: Handle Concurrent Requests with Conditional Writes

Use optimistic locking for safe concurrent updates.

```pseudocode
// adapters/secondary/database/versioned_repository

// ErrConcurrentModification indicates a version conflict.
CONSTANT ErrConcurrentModification = Error("concurrent modification detected")

// VersionedAsset includes version for optimistic locking.
TYPE VersionedAsset
    id: String
    version: Integer
    state: String
    currentLoad: Float

// UpdateAssetWithVersion performs an optimistic locking update.
// Only succeeds if the version matches, preventing lost updates.
METHOD AssetRepository.UpdateAssetWithVersion(ctx: Context, asset: VersionedAsset) RETURNS Result<Void, Error>
    newVersion = asset.version + 1

    // Build conditional expression: succeed only if version matches OR item doesn't exist
    condition = "pk NOT EXISTS OR version = :expectedVersion"
    conditionValues = Map{":expectedVersion": asset.version}

    // Update the version in the item
    asset.version = newVersion

    result = this.client.PutItemConditional(ctx, this.tableName, asset, condition, conditionValues)

    IF result.IsError() THEN
        IF result.Error().IsConditionFailed() THEN
            RETURN Error(ErrConcurrentModification)
        END IF
        RETURN Error("failed to update asset: " + result.Error())
    END IF

    RETURN Ok()
END METHOD
```

## Step 6: Implement Event Deduplication

Deduplicate events before processing to handle at-least-once delivery.

```pseudocode
// core/application/handlers/event_handler

// ProcessedEventStore tracks which events have been processed.
INTERFACE ProcessedEventStore
    Exists(ctx: Context, eventID: String) RETURNS Result<Boolean, Error>
    MarkProcessed(ctx: Context, eventID: String, ttl: Duration) RETURNS Result<Void, Error>
END INTERFACE

// AssetEventHandler handles asset events with deduplication.
TYPE AssetEventHandler
    processedStore: ProcessedEventStore
    assetRepo: AssetRepository

// EventEnvelope represents an incoming event.
TYPE EventEnvelope
    eventID: String
    eventType: String
    aggregateID: String
    occurredAt: Timestamp
    payload: Bytes

// Handle processes an event idempotently.
METHOD AssetEventHandler.Handle(ctx: Context, event: EventEnvelope) RETURNS Result<Void, Error>
    // Step 1: Check if already processed (deduplication lookup)
    existsResult = this.processedStore.Exists(ctx, event.eventID)
    IF existsResult.IsError() THEN
        RETURN Error("failed to check event status: " + existsResult.Error())
    END IF
    IF existsResult.Value() THEN
        // Already processed - return success (idempotent behavior)
        RETURN Ok()
    END IF

    // Step 2: Process the event based on type
    SWITCH event.eventType
        CASE "asset.state_changed":
            processResult = this.handleAssetStateChanged(ctx, event)
            IF processResult.IsError() THEN
                RETURN processResult.Error()
            END IF
        CASE "asset.registered":
            processResult = this.handleAssetRegistered(ctx, event)
            IF processResult.IsError() THEN
                RETURN processResult.Error()
            END IF
        DEFAULT:
            RETURN Error("unknown event type: " + event.eventType)
    END SWITCH

    // Step 3: Mark as processed with 30-day TTL
    markResult = this.processedStore.MarkProcessed(ctx, event.eventID, Duration(30 * Day))
    IF markResult.IsError() THEN
        // Log but don't fail - processing succeeded
        Log("warning: failed to mark event as processed: " + markResult.Error())
    END IF

    RETURN Ok()
END METHOD

// handleAssetStateChanged processes state change events.
METHOD AssetEventHandler.handleAssetStateChanged(ctx: Context, event: EventEnvelope) RETURNS Result<Void, Error>
    TYPE StateChangedPayload
        assetID: String
        facilityID: String
        state: String
        currentLoad: Float

    payload = Deserialize<StateChangedPayload>(event.payload)
    IF payload.IsError() THEN
        RETURN Error("failed to deserialize payload: " + payload.Error())
    END IF

    // Upsert the asset (idempotent operation)
    a = Asset{
        ID: payload.Value().assetID,
        FacilityID: payload.Value().facilityID,
        State: payload.Value().state,
        CurrentLoad: payload.Value().currentLoad
    }

    RETURN this.assetRepo.UpsertAsset(ctx, a)
END METHOD

// handleAssetRegistered processes asset registration events.
METHOD AssetEventHandler.handleAssetRegistered(ctx: Context, event: EventEnvelope) RETURNS Result<Void, Error>
    TYPE RegisteredPayload
        assetID: String
        facilityID: String
        assetType: String
        capacityKW: Float

    payload = Deserialize<RegisteredPayload>(event.payload)
    IF payload.IsError() THEN
        RETURN Error("failed to deserialize payload: " + payload.Error())
    END IF

    // Upsert creates or updates - idempotent by design
    a = Asset{
        ID: payload.Value().assetID,
        FacilityID: payload.Value().facilityID,
        Type: payload.Value().assetType,
        Capacity: payload.Value().capacityKW,
        State: StateOnline
    }

    RETURN this.assetRepo.UpsertAsset(ctx, a)
END METHOD
```

### Processed Event Store Implementation

```pseudocode
// adapters/secondary/database/processed_event_store

// ProcessedEventStore tracks processed events for deduplication.
TYPE ProcessedEventStoreAdapter
    client: DatabaseClient
    tableName: String

// NewProcessedEventStore creates a new processed event store.
CONSTRUCTOR NewProcessedEventStore(client: DatabaseClient, tableName: String) RETURNS ProcessedEventStoreAdapter
    RETURN ProcessedEventStoreAdapter{
        client: client,
        tableName: tableName
    }
END CONSTRUCTOR

// Exists checks if an event was already processed.
METHOD ProcessedEventStoreAdapter.Exists(ctx: Context, eventID: String) RETURNS Result<Boolean, Error>
    result = this.client.GetItem(ctx, this.tableName, eventID, "pk")  // ProjectionExpression
    IF result.IsError() THEN
        RETURN Error("failed to get item: " + result.Error())
    END IF

    RETURN Ok(result.Value() != NULL)
END METHOD

// MarkProcessed records an event as processed with TTL for cleanup.
METHOD ProcessedEventStoreAdapter.MarkProcessed(ctx: Context, eventID: String, ttl: Duration) RETURNS Result<Void, Error>
    item = Map{
        "pk": eventID,
        "processed_at": Now().Unix(),
        "ttl": Now().Add(ttl).Unix()
    }

    // PutItem is idempotent - safe to call multiple times
    result = this.client.PutItem(ctx, this.tableName, item)
    IF result.IsError() THEN
        RETURN Error("failed to put item: " + result.Error())
    END IF

    RETURN Ok()
END METHOD
```

## Step 7: Stateless Handler

Create a stateless handler that demonstrates all patterns.

```pseudocode
// cmd/api/main

// Handler holds all dependencies (stateless: no mutable state).
TYPE Handler
    eventHandler: AssetEventHandler

// Global handler instance - initialized once during cold start.
// No mutable state between invocations.
VARIABLE handler: Handler

// init runs once during cold start.
// All state comes from external sources (database, environment).
FUNCTION init()
    ctx = NewContext()

    cfg = LoadConfig()

    dbClient = NewDatabaseClient(cfg)

    // Create stores (they hold no state, just references)
    tableName = GetEnv("TABLE_NAME")
    processedStore = NewProcessedEventStore(dbClient, tableName)
    assetRepo = NewAssetRepository(dbClient, tableName)

    // Create handler
    handler = Handler{
        eventHandler: NewAssetEventHandler(processedStore, assetRepo)
    }
END FUNCTION

// Handle processes events statelessly.
METHOD Handler.Handle(ctx: Context, event: EventBridgeEvent) RETURNS Result<Void, Error>
    // Parse event envelope
    envelope = EventEnvelope{
        eventID: event.ID,
        eventType: event.DetailType,
        occurredAt: event.Time,
        payload: event.Detail
    }

    // Process idempotently - safe for retries
    RETURN this.eventHandler.Handle(ctx, envelope)
END METHOD

FUNCTION main()
    StartServerlessRuntime(handler.Handle)
END FUNCTION
```

## Verification Checklist

After implementing stateless and idempotent services, verify:

- [ ] No mutable global state between requests
- [ ] All state stored in external systems (database, cache, storage)
- [ ] Idempotency keys uniquely identify operations
- [ ] Deduplication lookup checks before processing
- [ ] Upsert operations used for persistence (PutItem)
- [ ] Conditional writes handle concurrent requests
- [ ] Event IDs checked for deduplication
- [ ] TTL configured for idempotency records (cleanup)
- [ ] Failed operations properly marked for retry visibility
- [ ] Same input always produces same output (deterministic)
- [ ] No in-memory caches that survive across invocations
- [ ] Middleware can be applied to any handler uniformly
