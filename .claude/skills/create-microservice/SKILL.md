---
name: create-microservice
description: "Step-by-step guide for scaffolding a new  microservice with Clean Architecture, Hexagonal, and DDD integration."
tools: ["Read", "Write", "Edit", "Bash", "Glob", "Grep"]
context:
  - type: file
    path: "architecture/**/*.md"
  - type: file
    path: "patterns/**/*.md"
---

# Skill: Create Microservice

This skill teaches you how to scaffold a complete microservice following  architectural patterns. You'll create a production-ready service structure combining Clean Architecture layers, Hexagonal ports and adapters, DDD patterns, and serverless deployment.

A well-structured microservice ensures maintainability, testability, and clear domain boundaries. Following consistent patterns across services enables team members to quickly understand and contribute to any service in the monorepo. This skill guides you through creating the complete scaffolding for a new bounded context.

The scaffolding you create will support multiple entry points (API, event handlers, scheduled tasks), proper dependency injection, contract-driven development with OpenAPI, and infrastructure-as-code deployment.

## Prerequisites

- Understanding of Clean Architecture, Hexagonal Architecture, and DDD concepts
- Bounded context identified and domain model designed
- Familiarity with serverless patterns and NoSQL databases

## Overview

In this skill, you will:
1. Initialize the module and create the monorepo directory structure
2. Create the domain layer with aggregates, value objects, events, and errors
3. Create the application layer with ports (inbound/outbound) and use cases
4. Create primary adapters (serverless handlers)
5. Create secondary adapters (repository, event publisher)
6. Set up the entry point with dependency injection
7. Define the OpenAPI contract
8. Create the Makefile for build, test, and deployment

## Step 1: Initialize Project Structure

Create the monorepo-style directory structure that follows Clean Architecture and Hexagonal patterns.

### Create Directory Structure

```bash
# Create service directory in the monorepo
mkdir -p services/asset-svc
cd services/asset-svc

# Create Clean Architecture layer directories
mkdir -p cmd/api
mkdir -p cmd/worker
mkdir -p cmd/eventhandler

# Create internal core (the Hexagon)
mkdir -p internal/core/domain/asset
mkdir -p internal/core/domain/services
mkdir -p internal/core/ports/inbound
mkdir -p internal/core/ports/outbound
mkdir -p internal/core/application

# Create adapters
mkdir -p internal/adapters/inbound/lambda
mkdir -p internal/adapters/inbound/http
mkdir -p internal/adapters/outbound/repository
mkdir -p internal/adapters/outbound/eventpublisher

# Create handlers
mkdir -p internal/handlers

# Create contracts and API specifications
mkdir -p api
mkdir -p contracts/events
mkdir -p contracts/mocks

# Create deployment and testing directories
mkdir -p deploy/iac/constructs
mkdir -p tests/unit
mkdir -p tests/service
mkdir -p tests/contract
mkdir -p tests/integration
```

### Directory Structure Explained

```
services/asset-svc/                       # One Bounded Context = One Microservice
├── api/
│   └── openapi.yaml                      # HTTP API contract (OpenAPI 3.x)
├── contracts/
│   ├── events/                           # Domain event schemas (JSON Schema)
│   │   ├── battery_registered.json
│   │   └── state_of_charge_updated.json
│   └── mocks/                            # Generated contract mocks
├── cmd/                                  # Entry points (handlers)
│   ├── api/main                          # API handler
│   ├── worker/main                       # Queue worker
│   └── eventhandler/main                 # Event handler
├── internal/
│   ├── handlers/                         # Handler implementations
│   │   ├── api_handler
│   │   └── event_handler
│   ├── core/                             # The Hexagon (Application Core)
│   │   ├── domain/                       # DDD Domain Model
│   │   │   ├── asset/                    # Aggregate boundary
│   │   │   │   ├── battery              # Aggregate Root
│   │   │   │   ├── capacity             # Value Object
│   │   │   │   ├── state_of_charge      # Value Object
│   │   │   │   ├── events               # Domain Events
│   │   │   │   └── errors               # Domain Errors
│   │   │   └── services/
│   │   │       └── asset_validator      # Domain Service
│   │   ├── ports/                        # Hexagonal Ports
│   │   │   ├── inbound/                  # Driving Ports (InPorts)
│   │   │   │   ├── commands
│   │   │   │   └── queries
│   │   │   └── outbound/                 # Driven Ports (OutPorts)
│   │   │       ├── repository
│   │   │       └── publisher
│   │   └── application/                  # Use Cases Layer
│   │       ├── service
│   │       ├── register_battery
│   │       └── update_soc
│   └── adapters/                         # Hexagonal Adapters
│       ├── inbound/
│       │   ├── http/handlers
│       │   └── lambda/handler
│       └── outbound/
│           ├── repository/adapter
│           └── eventpublisher/adapter
├── deploy/
│   └── iac/                              # Infrastructure as Code
│       ├── app
│       ├── stack
│       └── constructs/
├── tests/
│   ├── unit/
│   ├── service/
│   ├── contract/
│   └── integration/
└── Makefile
```

This structure provides clear separation between layers while keeping related code together. The `internal` package ensures encapsulation, and the `core` subdirectory emphasizes that this is the application hexagon.

## Step 2: Create Domain Layer

The domain layer contains all business rules and has zero dependencies on infrastructure. Start here because it forms the foundation.

### Domain Errors

```pseudocode
// internal/core/domain/asset/errors

// Domain errors define business rule violations.
// These errors are part of the ubiquitous language and should be
// meaningful to domain experts.

CONSTANT ErrAssetNotFound = Error("asset not found")
CONSTANT ErrAssetAlreadyExists = Error("asset already exists")
CONSTANT ErrInvalidAssetState = Error("operation not allowed in current asset state")
CONSTANT ErrInvalidCapacity = Error("capacity must be positive")
CONSTANT ErrInvalidStateOfCharge = Error("state of charge must be between 0 and 100")
CONSTANT ErrFacilityRequired = Error("facility ID is required")
CONSTANT ErrNameRequired = Error("asset name is required")
```

### Value Objects

Value objects are immutable and validated on construction. They encapsulate domain concepts and their invariants.

```pseudocode
// internal/core/domain/asset/capacity

// Capacity represents energy storage capacity in kWh.
// This is a Value Object - immutable and identified by its value.
TYPE Capacity
    kWh: Float

// NewCapacity creates a validated Capacity value object.
// Returns error if capacity violates business rules (must be positive).
CONSTRUCTOR NewCapacity(kWh: Float) RETURNS Result<Capacity, Error>
    IF kWh <= 0 THEN
        RETURN Error(ErrInvalidCapacity + ": got " + kWh + " kWh")
    END IF
    RETURN Ok(Capacity{kWh: kWh})
END CONSTRUCTOR

// MustCapacity creates a Capacity, panicking on invalid values.
// Use only in tests or when value is known to be valid.
CONSTRUCTOR MustCapacity(kWh: Float) RETURNS Capacity
    result = NewCapacity(kWh)
    IF result.IsError() THEN
        PANIC(result.Error())
    END IF
    RETURN result.Value()
END CONSTRUCTOR

// KWh returns the capacity value in kilowatt-hours.
METHOD Capacity.KWh() RETURNS Float
    RETURN this.kWh
END METHOD

// String provides human-readable representation.
METHOD Capacity.String() RETURNS String
    RETURN Format("%.2f kWh", this.kWh)
END METHOD
```

```pseudocode
// internal/core/domain/asset/state_of_charge

// StateOfCharge represents battery charge level as percentage (0-100).
// This is a Value Object with business invariant: must be between 0 and 100.
TYPE StateOfCharge
    percentage: Float

// NewStateOfCharge creates a validated StateOfCharge value object.
CONSTRUCTOR NewStateOfCharge(percentage: Float) RETURNS Result<StateOfCharge, Error>
    IF percentage < 0 OR percentage > 100 THEN
        RETURN Error(ErrInvalidStateOfCharge + ": got " + percentage + "%")
    END IF
    RETURN Ok(StateOfCharge{percentage: percentage})
END CONSTRUCTOR

// MustStateOfCharge creates a StateOfCharge, panicking on invalid values.
CONSTRUCTOR MustStateOfCharge(percentage: Float) RETURNS StateOfCharge
    result = NewStateOfCharge(percentage)
    IF result.IsError() THEN
        PANIC(result.Error())
    END IF
    RETURN result.Value()
END CONSTRUCTOR

// Percentage returns the SoC as a percentage value.
METHOD StateOfCharge.Percentage() RETURNS Float
    RETURN this.percentage
END METHOD

// IsLow returns true if charge is below 20%.
METHOD StateOfCharge.IsLow() RETURNS Boolean
    RETURN this.percentage < 20
END METHOD

// IsCritical returns true if charge is below 10%.
METHOD StateOfCharge.IsCritical() RETURNS Boolean
    RETURN this.percentage < 10
END METHOD

// String provides human-readable representation.
METHOD StateOfCharge.String() RETURNS String
    RETURN Format("%.1f%%", this.percentage)
END METHOD
```

### Domain Events

Domain events capture important state changes. The aggregate raises events but doesn't publish them directly.

```pseudocode
// internal/core/domain/asset/events

// Event is the base interface for all domain events.
INTERFACE Event
    EventType() RETURNS String
    OccurredAt() RETURNS Timestamp
    AggregateID() RETURNS String
END INTERFACE

// BaseEvent provides common event fields.
TYPE BaseEvent
    occurredAt: Timestamp
    aggregateID: String

METHOD BaseEvent.OccurredAt() RETURNS Timestamp
    RETURN this.occurredAt
END METHOD

METHOD BaseEvent.AggregateID() RETURNS String
    RETURN this.aggregateID
END METHOD

// BatteryRegistered is raised when a new battery is registered.
TYPE BatteryRegistered
    EXTENDS BaseEvent
    facilityID: String
    name: String
    capacityKWh: Float

METHOD BatteryRegistered.EventType() RETURNS String
    RETURN "asset.battery.registered"
END METHOD

// NewBatteryRegistered creates the event.
CONSTRUCTOR NewBatteryRegistered(id: String, facilityID: String, name: String, capacityKWh: Float) RETURNS BatteryRegistered
    RETURN BatteryRegistered{
        BaseEvent: BaseEvent{occurredAt: Now(), aggregateID: id},
        facilityID: facilityID,
        name: name,
        capacityKWh: capacityKWh
    }
END CONSTRUCTOR

// StateOfChargeUpdated is raised when battery SoC changes.
TYPE StateOfChargeUpdated
    EXTENDS BaseEvent
    previousSoC: Float
    newSoC: Float

METHOD StateOfChargeUpdated.EventType() RETURNS String
    RETURN "asset.battery.soc_updated"
END METHOD

// NewStateOfChargeUpdated creates the event.
CONSTRUCTOR NewStateOfChargeUpdated(id: String, previousSoC: Float, newSoC: Float) RETURNS StateOfChargeUpdated
    RETURN StateOfChargeUpdated{
        BaseEvent: BaseEvent{occurredAt: Now(), aggregateID: id},
        previousSoC: previousSoC,
        newSoC: newSoC
    }
END CONSTRUCTOR

// BatteryStateChanged is raised when operational state changes.
TYPE BatteryStateChanged
    EXTENDS BaseEvent
    previousState: AssetState
    newState: AssetState
    reason: String

METHOD BatteryStateChanged.EventType() RETURNS String
    RETURN "asset.battery.state_changed"
END METHOD
```

### Aggregate Root

The aggregate root is the entry point for all operations on the aggregate. It enforces all invariants and raises domain events.

```pseudocode
// internal/core/domain/asset/battery

// AssetState represents the operational state of an asset.
TYPE AssetState = String

CONSTANT AssetStateOnline: AssetState = "online"
CONSTANT AssetStateOffline: AssetState = "offline"
CONSTANT AssetStateFault: AssetState = "fault"
CONSTANT AssetStateMaintenance: AssetState = "maintenance"

// Battery is the Aggregate Root for battery assets.
// All state changes go through methods that enforce invariants.
TYPE Battery
    id: String
    facilityID: String
    name: String
    capacity: Capacity
    soc: StateOfCharge
    state: AssetState
    registeredAt: Timestamp
    updatedAt: Timestamp
    uncommittedEvents: List<Event>  // Events raised during this transaction

// NewBattery creates and registers a new Battery aggregate.
// This is the factory method that enforces creation invariants.
CONSTRUCTOR NewBattery(facilityID: String, name: String, capacity: Capacity) RETURNS Result<Battery, Error>
    IF facilityID == "" THEN
        RETURN Error(ErrFacilityRequired)
    END IF
    IF name == "" THEN
        RETURN Error(ErrNameRequired)
    END IF

    id = GenerateUUID()
    now = Now()
    initialSoC = MustStateOfCharge(50)  // Start at 50%

    battery = Battery{
        id: id,
        facilityID: facilityID,
        name: name,
        capacity: capacity,
        soc: initialSoC,
        state: AssetStateOnline,
        registeredAt: now,
        updatedAt: now,
        uncommittedEvents: []
    }

    // Raise domain event for registration
    battery.raise(NewBatteryRegistered(id, facilityID, name, capacity.KWh()))

    RETURN Ok(battery)
END CONSTRUCTOR

// ReconstituteBattery recreates a Battery from persisted state.
// Used by repositories to rebuild the aggregate. Does not raise events.
CONSTRUCTOR ReconstituteBattery(
    id: String, facilityID: String, name: String,
    capacity: Capacity, soc: StateOfCharge, state: AssetState,
    registeredAt: Timestamp, updatedAt: Timestamp
) RETURNS Battery
    RETURN Battery{
        id: id,
        facilityID: facilityID,
        name: name,
        capacity: capacity,
        soc: soc,
        state: state,
        registeredAt: registeredAt,
        updatedAt: updatedAt,
        uncommittedEvents: []
    }
END CONSTRUCTOR

// Accessor methods
METHOD Battery.ID() RETURNS String
    RETURN this.id
END METHOD

METHOD Battery.FacilityID() RETURNS String
    RETURN this.facilityID
END METHOD

METHOD Battery.Name() RETURNS String
    RETURN this.name
END METHOD

METHOD Battery.Capacity() RETURNS Capacity
    RETURN this.capacity
END METHOD

METHOD Battery.StateOfCharge() RETURNS StateOfCharge
    RETURN this.soc
END METHOD

METHOD Battery.State() RETURNS AssetState
    RETURN this.state
END METHOD

METHOD Battery.RegisteredAt() RETURNS Timestamp
    RETURN this.registeredAt
END METHOD

METHOD Battery.UpdatedAt() RETURNS Timestamp
    RETURN this.updatedAt
END METHOD

// UpdateStateOfCharge updates the SoC, enforcing the 0-100 invariant.
// Returns error if battery is in fault state.
METHOD Battery.UpdateStateOfCharge(newSoC: StateOfCharge) RETURNS Result<Void, Error>
    IF this.state == AssetStateFault THEN
        RETURN Error(ErrInvalidAssetState + ": battery is in fault state")
    END IF
    IF this.state == AssetStateMaintenance THEN
        RETURN Error(ErrInvalidAssetState + ": battery is under maintenance")
    END IF

    previousSoC = this.soc
    this.soc = newSoC
    this.updatedAt = Now()

    this.raise(NewStateOfChargeUpdated(this.id, previousSoC.Percentage(), newSoC.Percentage()))

    RETURN Ok()
END METHOD

// SetState changes the operational state with reason tracking.
METHOD Battery.SetState(newState: AssetState, reason: String) RETURNS Result<Void, Error>
    IF this.state == newState THEN
        RETURN Ok()  // No change needed
    END IF

    previousState = this.state
    this.state = newState
    this.updatedAt = Now()

    this.raise(BatteryStateChanged{
        BaseEvent: BaseEvent{occurredAt: this.updatedAt, aggregateID: this.id},
        previousState: previousState,
        newState: newState,
        reason: reason
    })

    RETURN Ok()
END METHOD

// raise adds a domain event to the uncommitted list.
METHOD Battery.raise(event: Event)
    this.uncommittedEvents = Append(this.uncommittedEvents, event)
END METHOD

// UncommittedEvents returns events raised but not yet persisted.
METHOD Battery.UncommittedEvents() RETURNS List<Event>
    RETURN this.uncommittedEvents
END METHOD

// ClearUncommittedEvents clears events after successful persistence.
METHOD Battery.ClearUncommittedEvents()
    this.uncommittedEvents = []
END METHOD
```

## Step 3: Create Application Layer

The application layer contains ports (interfaces) and use cases that orchestrate domain operations.

### Output Ports (Driven Interfaces)

```pseudocode
// internal/core/ports/outbound/repository

// BatteryRepository defines persistence operations for Battery aggregates.
// This is an OutPort - the interface is owned by the application,
// implementations are provided by adapters.
INTERFACE BatteryRepository
    // Save persists a battery aggregate.
    Save(ctx: Context, battery: Battery) RETURNS Result<Void, Error>

    // FindByID retrieves a battery by its unique identifier.
    FindByID(ctx: Context, id: String) RETURNS Result<Battery, Error>

    // FindByFacility retrieves all batteries for a facility.
    FindByFacility(ctx: Context, facilityID: String) RETURNS Result<List<Battery>, Error>

    // Delete removes a battery from persistence.
    Delete(ctx: Context, id: String) RETURNS Result<Void, Error>
END INTERFACE
```

```pseudocode
// internal/core/ports/outbound/publisher

// EventPublisher defines the interface for publishing domain events.
// Implementations handle the infrastructure details (message bus, queue, etc).
INTERFACE EventPublisher
    // Publish sends domain events to the event bus.
    Publish(ctx: Context, events: List<Event>) RETURNS Result<Void, Error>
END INTERFACE
```

### Input Ports (Commands and Queries)

```pseudocode
// internal/core/ports/inbound/commands

// RegisterBatteryCommand contains input for battery registration.
TYPE RegisterBatteryCommand
    facilityID: String
    name: String
    capacityKWh: Float

// RegisterBatteryResult contains output from registration.
TYPE RegisterBatteryResult
    batteryID: String

// UpdateStateOfChargeCommand contains input for SoC update.
TYPE UpdateStateOfChargeCommand
    batteryID: String
    newPercentage: Float

// BatteryCommandHandler defines the command handling interface.
// This is an InPort - driving adapters call these methods.
INTERFACE BatteryCommandHandler
    RegisterBattery(ctx: Context, cmd: RegisterBatteryCommand) RETURNS Result<RegisterBatteryResult, Error>
    UpdateStateOfCharge(ctx: Context, cmd: UpdateStateOfChargeCommand) RETURNS Result<Void, Error>
END INTERFACE
```

```pseudocode
// internal/core/ports/inbound/queries

// BatteryDTO is the read model for battery queries.
TYPE BatteryDTO
    id: String
    facilityID: String
    name: String
    capacityKWh: Float
    socPercent: Float
    state: String
    registeredAt: Timestamp
    updatedAt: Timestamp

// BatteryQueryHandler defines the query handling interface.
INTERFACE BatteryQueryHandler
    GetBattery(ctx: Context, id: String) RETURNS Result<BatteryDTO, Error>
    ListBatteriesByFacility(ctx: Context, facilityID: String) RETURNS Result<List<BatteryDTO>, Error>
END INTERFACE
```

### Use Cases (Application Service)

```pseudocode
// internal/core/application/service

// BatteryService implements both command and query handlers.
// It orchestrates domain operations without containing business logic.
TYPE BatteryService
    repo: BatteryRepository
    publisher: EventPublisher

// NewBatteryService creates the application service with dependencies.
CONSTRUCTOR NewBatteryService(repo: BatteryRepository, publisher: EventPublisher) RETURNS BatteryService
    RETURN BatteryService{
        repo: repo,
        publisher: publisher
    }
END CONSTRUCTOR

// RegisterBattery handles the battery registration use case.
METHOD BatteryService.RegisterBattery(ctx: Context, cmd: RegisterBatteryCommand) RETURNS Result<RegisterBatteryResult, Error>
    // Create validated value object
    capacityResult = NewCapacity(cmd.capacityKWh)
    IF capacityResult.IsError() THEN
        RETURN Error("invalid capacity: " + capacityResult.Error())
    END IF
    capacity = capacityResult.Value()

    // Create aggregate (factory enforces invariants)
    batteryResult = NewBattery(cmd.facilityID, cmd.name, capacity)
    IF batteryResult.IsError() THEN
        RETURN Error("failed to create battery: " + batteryResult.Error())
    END IF
    battery = batteryResult.Value()

    // Persist aggregate
    saveResult = this.repo.Save(ctx, battery)
    IF saveResult.IsError() THEN
        RETURN Error("failed to save battery: " + saveResult.Error())
    END IF

    // Publish domain events
    publishResult = this.publisher.Publish(ctx, battery.UncommittedEvents())
    IF publishResult.IsError() THEN
        // Log warning but don't fail - events can be replayed
        // In production, consider transactional outbox pattern
        Log("warning: failed to publish events: " + publishResult.Error())
    END IF

    battery.ClearUncommittedEvents()

    RETURN Ok(RegisterBatteryResult{batteryID: battery.ID()})
END METHOD

// UpdateStateOfCharge handles SoC update use case.
METHOD BatteryService.UpdateStateOfCharge(ctx: Context, cmd: UpdateStateOfChargeCommand) RETURNS Result<Void, Error>
    // Load aggregate
    batteryResult = this.repo.FindByID(ctx, cmd.batteryID)
    IF batteryResult.IsError() THEN
        RETURN Error("failed to find battery: " + batteryResult.Error())
    END IF
    battery = batteryResult.Value()

    // Create validated value object
    socResult = NewStateOfCharge(cmd.newPercentage)
    IF socResult.IsError() THEN
        RETURN Error("invalid state of charge: " + socResult.Error())
    END IF
    newSoC = socResult.Value()

    // Execute domain operation (aggregate enforces invariants)
    updateResult = battery.UpdateStateOfCharge(newSoC)
    IF updateResult.IsError() THEN
        RETURN Error("failed to update SoC: " + updateResult.Error())
    END IF

    // Persist changes
    saveResult = this.repo.Save(ctx, battery)
    IF saveResult.IsError() THEN
        RETURN Error("failed to save battery: " + saveResult.Error())
    END IF

    // Publish events
    publishResult = this.publisher.Publish(ctx, battery.UncommittedEvents())
    IF publishResult.IsError() THEN
        Log("warning: failed to publish events: " + publishResult.Error())
    END IF

    battery.ClearUncommittedEvents()

    RETURN Ok()
END METHOD

// GetBattery retrieves a single battery.
METHOD BatteryService.GetBattery(ctx: Context, id: String) RETURNS Result<BatteryDTO, Error>
    batteryResult = this.repo.FindByID(ctx, id)
    IF batteryResult.IsError() THEN
        RETURN batteryResult.Error()
    END IF

    RETURN Ok(this.toDTO(batteryResult.Value()))
END METHOD

// ListBatteriesByFacility retrieves all batteries for a facility.
METHOD BatteryService.ListBatteriesByFacility(ctx: Context, facilityID: String) RETURNS Result<List<BatteryDTO>, Error>
    batteriesResult = this.repo.FindByFacility(ctx, facilityID)
    IF batteriesResult.IsError() THEN
        RETURN batteriesResult.Error()
    END IF

    batteries = batteriesResult.Value()
    dtos = []
    FOR EACH b IN batteries DO
        dtos = Append(dtos, this.toDTO(b))
    END FOR

    RETURN Ok(dtos)
END METHOD

METHOD BatteryService.toDTO(b: Battery) RETURNS BatteryDTO
    RETURN BatteryDTO{
        id: b.ID(),
        facilityID: b.FacilityID(),
        name: b.Name(),
        capacityKWh: b.Capacity().KWh(),
        socPercent: b.StateOfCharge().Percentage(),
        state: b.State(),
        registeredAt: b.RegisteredAt(),
        updatedAt: b.UpdatedAt()
    }
END METHOD
```

## Step 4: Create Primary Adapters (Serverless Handlers)

Primary adapters drive the application. They translate external input into commands/queries.

```pseudocode
// internal/adapters/inbound/lambda/handler

// APIHandler handles API Gateway requests.
TYPE APIHandler
    commands: BatteryCommandHandler
    queries: BatteryQueryHandler

// NewAPIHandler creates the handler with dependencies.
CONSTRUCTOR NewAPIHandler(commands: BatteryCommandHandler, queries: BatteryQueryHandler) RETURNS APIHandler
    RETURN APIHandler{
        commands: commands,
        queries: queries
    }
END CONSTRUCTOR

// Handle processes API Gateway proxy requests.
METHOD APIHandler.Handle(ctx: Context, request: APIRequest) RETURNS Result<APIResponse, Error>
    SWITCH
        CASE request.Method == "POST" AND request.Resource == "/batteries":
            RETURN this.registerBattery(ctx, request)
        CASE request.Method == "GET" AND request.Resource == "/batteries/{id}":
            RETURN this.getBattery(ctx, request)
        CASE request.Method == "PUT" AND request.Resource == "/batteries/{id}/soc":
            RETURN this.updateSoC(ctx, request)
        DEFAULT:
            RETURN this.notFound()
    END SWITCH
END METHOD

METHOD APIHandler.registerBattery(ctx: Context, request: APIRequest) RETURNS Result<APIResponse, Error>
    // Parse request body
    TYPE RequestBody
        facilityID: String
        name: String
        capacityKWh: Float

    body = ParseJSON<RequestBody>(request.Body)
    IF body.IsError() THEN
        RETURN this.badRequest("invalid request body: " + body.Error())
    END IF

    result = this.commands.RegisterBattery(ctx, RegisterBatteryCommand{
        facilityID: body.Value().facilityID,
        name: body.Value().name,
        capacityKWh: body.Value().capacityKWh
    })
    IF result.IsError() THEN
        RETURN this.handleError(result.Error())
    END IF

    RETURN this.created(Map{"battery_id": result.Value().batteryID})
END METHOD

METHOD APIHandler.getBattery(ctx: Context, request: APIRequest) RETURNS Result<APIResponse, Error>
    id = request.PathParameters["id"]

    result = this.queries.GetBattery(ctx, id)
    IF result.IsError() THEN
        RETURN this.handleError(result.Error())
    END IF

    RETURN this.ok(result.Value())
END METHOD

METHOD APIHandler.updateSoC(ctx: Context, request: APIRequest) RETURNS Result<APIResponse, Error>
    id = request.PathParameters["id"]

    TYPE RequestBody
        percentage: Float

    body = ParseJSON<RequestBody>(request.Body)
    IF body.IsError() THEN
        RETURN this.badRequest("invalid request body: " + body.Error())
    END IF

    result = this.commands.UpdateStateOfCharge(ctx, UpdateStateOfChargeCommand{
        batteryID: id,
        newPercentage: body.Value().percentage
    })
    IF result.IsError() THEN
        RETURN this.handleError(result.Error())
    END IF

    RETURN this.noContent()
END METHOD

METHOD APIHandler.ok(body: Any) RETURNS Result<APIResponse, Error>
    RETURN this.jsonResponse(200, body)
END METHOD

METHOD APIHandler.created(body: Any) RETURNS Result<APIResponse, Error>
    RETURN this.jsonResponse(201, body)
END METHOD

METHOD APIHandler.noContent() RETURNS Result<APIResponse, Error>
    RETURN Ok(APIResponse{statusCode: 204})
END METHOD

METHOD APIHandler.badRequest(message: String) RETURNS Result<APIResponse, Error>
    RETURN this.jsonResponse(400, Map{"error": message})
END METHOD

METHOD APIHandler.notFound() RETURNS Result<APIResponse, Error>
    RETURN this.jsonResponse(404, Map{"error": "not found"})
END METHOD

METHOD APIHandler.handleError(err: Error) RETURNS Result<APIResponse, Error>
    // Map domain errors to HTTP status codes
    RETURN this.jsonResponse(500, Map{"error": err.Message()})
END METHOD

METHOD APIHandler.jsonResponse(status: Integer, body: Any) RETURNS Result<APIResponse, Error>
    data = ToJSON(body)
    RETURN Ok(APIResponse{
        statusCode: status,
        headers: Map{"Content-Type": "application/json"},
        body: data
    })
END METHOD
```

## Step 5: Create Secondary Adapters

Secondary adapters implement outbound ports for infrastructure integration.

### Repository Adapter

```pseudocode
// internal/adapters/outbound/repository/adapter

// BatteryRepository implements outbound.BatteryRepository with database storage.
TYPE BatteryRepositoryAdapter
    client: DatabaseClient
    tableName: String

// NewBatteryRepository creates the repository adapter.
CONSTRUCTOR NewBatteryRepository(client: DatabaseClient, tableName: String) RETURNS BatteryRepositoryAdapter
    RETURN BatteryRepositoryAdapter{client: client, tableName: tableName}
END CONSTRUCTOR

// batteryItem is the database representation.
TYPE batteryItem
    PK: String           // Primary key: "BATTERY#{id}"
    SK: String           // Sort key: "METADATA"
    GSI1PK: String       // GSI: "FACILITY#{facilityID}"
    GSI1SK: String       // GSI: "BATTERY#{id}"
    facilityID: String
    name: String
    capacityKWh: Float
    socPercent: Float
    state: String
    registeredAt: String
    updatedAt: String

// Save persists the battery aggregate.
METHOD BatteryRepositoryAdapter.Save(ctx: Context, battery: Battery) RETURNS Result<Void, Error>
    item = batteryItem{
        PK: "BATTERY#" + battery.ID(),
        SK: "METADATA",
        GSI1PK: "FACILITY#" + battery.FacilityID(),
        GSI1SK: "BATTERY#" + battery.ID(),
        facilityID: battery.FacilityID(),
        name: battery.Name(),
        capacityKWh: battery.Capacity().KWh(),
        socPercent: battery.StateOfCharge().Percentage(),
        state: battery.State(),
        registeredAt: FormatTimestamp(battery.RegisteredAt()),
        updatedAt: FormatTimestamp(battery.UpdatedAt())
    }

    result = this.client.PutItem(ctx, this.tableName, item)
    IF result.IsError() THEN
        RETURN Error("failed to put item: " + result.Error())
    END IF

    RETURN Ok()
END METHOD

// FindByID retrieves a battery by its unique identifier.
METHOD BatteryRepositoryAdapter.FindByID(ctx: Context, id: String) RETURNS Result<Battery, Error>
    result = this.client.GetItem(ctx, this.tableName, Map{
        "PK": "BATTERY#" + id,
        "SK": "METADATA"
    })
    IF result.IsError() THEN
        RETURN Error("failed to get item: " + result.Error())
    END IF

    IF result.Value() == NULL THEN
        RETURN Error(ErrAssetNotFound)
    END IF

    item = result.Value()
    RETURN this.toDomain(item)
END METHOD

// FindByFacility retrieves all batteries for a facility using GSI.
METHOD BatteryRepositoryAdapter.FindByFacility(ctx: Context, facilityID: String) RETURNS Result<List<Battery>, Error>
    result = this.client.Query(ctx, this.tableName, "GSI1", Map{
        "GSI1PK": "FACILITY#" + facilityID
    })
    IF result.IsError() THEN
        RETURN Error("failed to query: " + result.Error())
    END IF

    batteries = []
    FOR EACH item IN result.Value() DO
        batteryResult = this.toDomain(item)
        IF batteryResult.IsError() THEN
            RETURN batteryResult.Error()
        END IF
        batteries = Append(batteries, batteryResult.Value())
    END FOR

    RETURN Ok(batteries)
END METHOD

// Delete removes a battery from the database.
METHOD BatteryRepositoryAdapter.Delete(ctx: Context, id: String) RETURNS Result<Void, Error>
    result = this.client.DeleteItem(ctx, this.tableName, Map{
        "PK": "BATTERY#" + id,
        "SK": "METADATA"
    })
    RETURN result
END METHOD

METHOD BatteryRepositoryAdapter.toDomain(item: batteryItem) RETURNS Result<Battery, Error>
    capacityResult = NewCapacity(item.capacityKWh)
    IF capacityResult.IsError() THEN
        RETURN capacityResult.Error()
    END IF

    socResult = NewStateOfCharge(item.socPercent)
    IF socResult.IsError() THEN
        RETURN socResult.Error()
    END IF

    registeredAt = ParseTimestamp(item.registeredAt)
    updatedAt = ParseTimestamp(item.updatedAt)

    // Extract ID from PK (remove "BATTERY#" prefix)
    id = Substring(item.PK, 8)

    RETURN Ok(ReconstituteBattery(
        id,
        item.facilityID,
        item.name,
        capacityResult.Value(),
        socResult.Value(),
        AssetState(item.state),
        registeredAt,
        updatedAt
    ))
END METHOD
```

### Event Publisher Adapter

```pseudocode
// internal/adapters/outbound/eventpublisher/adapter

// EventPublisher implements outbound.EventPublisher with message bus.
TYPE EventPublisherAdapter
    client: MessageBusClient
    busName: String
    source: String

// NewEventPublisher creates the publisher adapter.
CONSTRUCTOR NewEventPublisher(client: MessageBusClient, busName: String) RETURNS EventPublisherAdapter
    RETURN EventPublisherAdapter{
        client: client,
        busName: busName,
        source: ".asset-svc"
    }
END CONSTRUCTOR

// Publish sends domain events to the message bus.
METHOD EventPublisherAdapter.Publish(ctx: Context, events: List<Event>) RETURNS Result<Void, Error>
    IF Length(events) == 0 THEN
        RETURN Ok()
    END IF

    entries = []
    FOR EACH event IN events DO
        detail = ToJSON(event)
        IF detail.IsError() THEN
            RETURN Error("failed to marshal event: " + detail.Error())
        END IF

        entries = Append(entries, EventEntry{
            busName: this.busName,
            source: this.source,
            detailType: event.EventType(),
            detail: detail.Value()
        })
    END FOR

    result = this.client.PublishEvents(ctx, entries)
    IF result.IsError() THEN
        RETURN Error("failed to publish events: " + result.Error())
    END IF

    IF result.Value().failedCount > 0 THEN
        RETURN Error("failed to publish " + result.Value().failedCount + " events")
    END IF

    RETURN Ok()
END METHOD
```

## Step 6: Create Entry Point

The entry point sets up dependency injection and starts the runtime.

```pseudocode
// cmd/api/main

// Global handler instance
VARIABLE handler: APIHandler

// init runs once during cold start - set up all dependencies here.
// This is the composition root where we wire everything together.
FUNCTION init()
    ctx = NewContext()

    // Load configuration
    cfg = LoadConfig()

    // Create infrastructure clients
    dbClient = NewDatabaseClient(cfg)
    messageBusClient = NewMessageBusClient(cfg)

    // Read configuration from environment
    tableName = GetEnv("TABLE_NAME")
    IF tableName == "" THEN
        Fatal("TABLE_NAME environment variable is required")
    END IF

    busName = GetEnv("EVENT_BUS_NAME")
    IF busName == "" THEN
        Fatal("EVENT_BUS_NAME environment variable is required")
    END IF

    // Create secondary adapters (outbound ports implementation)
    repo = NewBatteryRepository(dbClient, tableName)
    publisher = NewEventPublisher(messageBusClient, busName)

    // Create application service (use cases)
    service = NewBatteryService(repo, publisher)

    // Create primary adapter (inbound handler)
    handler = NewAPIHandler(service, service)
END FUNCTION

FUNCTION main()
    StartServerlessRuntime(handler.Handle)
END FUNCTION
```

## Step 7: Define OpenAPI Contract

```yaml
# api/openapi.yaml
openapi: 3.0.3
info:
  title: Asset Context API
  version: 1.0.0
  description: |
    API for managing energy assets in the Asset bounded context.
    This service owns all battery, PV, and EV charger assets.

servers:
  - url: https://api.example.io/v1/assets
    description: Production
  - url: https://api.staging.example.io/v1/assets
    description: Staging

paths:
  /batteries:
    post:
      summary: Register a new battery
      operationId: registerBattery
      tags:
        - Batteries
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/RegisterBatteryRequest'
      responses:
        '201':
          description: Battery registered successfully
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/RegisterBatteryResponse'
        '400':
          description: Invalid request
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
        '409':
          description: Battery already exists

  /batteries/{batteryId}:
    get:
      summary: Get battery details
      operationId: getBattery
      tags:
        - Batteries
      parameters:
        - name: batteryId
          in: path
          required: true
          schema:
            type: string
            format: uuid
      responses:
        '200':
          description: Battery details
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Battery'
        '404':
          description: Battery not found

  /batteries/{batteryId}/soc:
    put:
      summary: Update battery state of charge
      operationId: updateStateOfCharge
      tags:
        - Batteries
      parameters:
        - name: batteryId
          in: path
          required: true
          schema:
            type: string
            format: uuid
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/UpdateSoCRequest'
      responses:
        '204':
          description: State of charge updated
        '400':
          description: Invalid request
        '404':
          description: Battery not found

components:
  schemas:
    RegisterBatteryRequest:
      type: object
      required:
        - facility_id
        - name
        - capacity_kwh
      properties:
        facility_id:
          type: string
          format: uuid
          description: ID of the facility this battery belongs to
        name:
          type: string
          minLength: 1
          maxLength: 100
          description: Human-readable name for the battery
        capacity_kwh:
          type: number
          format: double
          minimum: 0.1
          description: Energy storage capacity in kilowatt-hours

    RegisterBatteryResponse:
      type: object
      properties:
        battery_id:
          type: string
          format: uuid

    Battery:
      type: object
      properties:
        id:
          type: string
          format: uuid
        facility_id:
          type: string
          format: uuid
        name:
          type: string
        capacity_kwh:
          type: number
          format: double
        soc_percent:
          type: number
          format: double
          minimum: 0
          maximum: 100
        state:
          type: string
          enum: [online, offline, fault, maintenance]
        registered_at:
          type: string
          format: date-time
        updated_at:
          type: string
          format: date-time

    UpdateSoCRequest:
      type: object
      required:
        - percentage
      properties:
        percentage:
          type: number
          format: double
          minimum: 0
          maximum: 100

    ErrorResponse:
      type: object
      properties:
        error:
          type: string
        code:
          type: string
```

## Step 8: Create Makefile

```makefile
# Makefile
.PHONY: build test lint deploy clean fmt vet

# Build parameters
BINARY_NAME=bootstrap
BUILD_DIR=.build

# Handlers
HANDLERS=api worker eventhandler

# Build all handlers
build: clean
	@for handler in $(HANDLERS); do \
		echo "Building $$handler..."; \
		# Build command depends on your technology stack \
		# Example: compile and package for deployment \
	done

# Build single handler
build-%:
	@echo "Building $*..."
	@mkdir -p $(BUILD_DIR)/$*
	# Build command for specific handler

# Run all tests
test:
	# Run tests with coverage
	# Example: test runner with race detection and coverage

# Run unit tests only
test-unit:
	# Run unit tests only (fast)

# Run integration tests
test-integration:
	# Run integration tests

# Run contract tests
test-contract:
	# Run contract tests

# Format code
fmt:
	# Format source code

# Vet code
vet:
	# Static analysis

# Run linter
lint:
	# Run linter

# Generate mocks
generate:
	# Generate mocks and other artifacts

# Deploy with IaC
deploy: build
	# Deploy using infrastructure as code

# Deploy to specific environment
deploy-%: build
	# Deploy to specific environment (e.g., staging, production)

# Synthesize IaC
synth:
	# Synthesize infrastructure templates

# Show IaC diff
diff:
	# Show infrastructure changes

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR)
	rm -f coverage.out

# Run locally
local:
	# Run service locally for development

# Validate OpenAPI spec
validate-api:
	# Validate OpenAPI specification

# Generate API documentation
docs:
	# Generate API documentation from OpenAPI spec
```

## Verification Checklist

After creating your microservice, verify the following:

### Structure
- [ ] Directory structure follows Clean Architecture layers (`cmd/`, `internal/core/`, `internal/adapters/`)
- [ ] The hexagon is clearly defined in `internal/core/`
- [ ] Ports are in `internal/core/ports/` with `inbound/` and `outbound/` separation
- [ ] Adapters are in `internal/adapters/` with `inbound/` and `outbound/` separation

### Domain Layer
- [ ] Domain layer (`internal/core/domain/`) has zero infrastructure imports
- [ ] All value objects validate on construction and are immutable
- [ ] Aggregate root enforces all business invariants
- [ ] Domain events are raised from aggregate methods
- [ ] Domain errors are meaningful and part of ubiquitous language

### Application Layer
- [ ] Repository and publisher interfaces defined in `ports/outbound/`
- [ ] Command and query interfaces defined in `ports/inbound/`
- [ ] Use cases orchestrate domain operations without business logic
- [ ] Application service implements both command and query handlers

### Adapters
- [ ] Adapters are thin (mapping, serialization, no business logic)
- [ ] Repository adapter maps between domain and persistence models
- [ ] Event publisher adapter handles event serialization
- [ ] Handler translates HTTP to commands/queries

### Entry Points
- [ ] Entry point `init()` sets up all dependencies (composition root)
- [ ] Handler delegates to application service
- [ ] Environment variables used for configuration

### Contracts
- [ ] OpenAPI contract matches implementation
- [ ] Event schemas defined in `contracts/events/`
- [ ] Request/response models documented

### Build
- [ ] Makefile has `build`, `test`, `lint`, `deploy`, `clean` targets
- [ ] Tests can run without cloud credentials (mocked adapters)
- [ ] Code compiles successfully
