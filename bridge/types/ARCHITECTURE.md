# Bridge Architecture

This document describes the architecture of the gobridge message bridge system.

## System Overview

The bridge provides a flexible, type-safe abstraction for connecting message sources to targets through configurable pipelines.

```mermaid
graph TB
    subgraph types [bridge/types]
        Config[Config Interfaces]
        Connection[Connection]
        Source[Source]
        Target[Target]
        Pipeline[Pipeline]
        Middleware[Middleware]
    end
    
    subgraph core [bridge/core]
        Bridge[Bridge]
        PipelineImpl[PipelineImpl]
        Registries[Registries]
    end
    
    subgraph transport [transport/mqtt]
        MQTTConn[MQTTConnection]
        MQTTSrc[Source]
        MQTTTgt[Target]
    end
    
    core -->|implements| types
    transport -->|implements| types
    core -->|uses| transport
```

## Interface Hierarchy

The type system is designed around composable interfaces:

```mermaid
classDiagram
    class Config {
        <<interface>>
        +GetID() string
        +GetTransportType() TransportType
    }
    
    class ConnectionConfig {
        <<interface>>
        +GetBridgeID() string
    }
    
    class SourceConfig {
        <<interface>>
        +GetQoS() QosLevel
        +GetPrefetch() int
    }
    
    class TargetConfig {
        <<interface>>
        +GetDefaultQoS() QosLevel
        +GetBatchSize() int
    }
    
    Config <|-- ConnectionConfig
    Config <|-- SourceConfig
    Config <|-- TargetConfig
    
    class Connection {
        <<interface>>
        +Start(ctx, config) error
        +Close() error
        +SourceProvider() SourceProvider
        +TargetProvider() TargetProvider
        +LifecycleCoordinator() LifecycleCoordinator
    }
    
    class Source {
        <<interface>>
        +Start(ctx) error
        +Messages() chan SourceMessage
        +Capabilities() Capabilities
    }
    
    class Target {
        <<interface>>
        +Send(ctx, msg) error
        +SendBatch(ctx, msgs) error
        +Capabilities() Capabilities
    }
```

## Message Flow

Messages flow from Source through Middleware to Target:

```mermaid
sequenceDiagram
    participant Broker as External Broker
    participant Conn as Connection
    participant Src as Source
    participant MW as Middleware
    participant Tgt as Target
    participant Dest as Destination
    
    Broker->>Conn: message arrives
    Conn->>Src: route to source
    Src->>Src: create SourceMessage
    Src-->>MW: Messages channel
    MW->>MW: process chain
    MW->>Tgt: Send msg
    Tgt->>Dest: publish
    Dest-->>Tgt: ack
    Tgt-->>MW: nil
    MW-->>Src: success
    Src->>Src: Ack
```

## Connection Architecture

Connections can provide Sources and Targets that share the underlying transport:

```mermaid
flowchart TB
    subgraph MQTTConnection [MQTT Connection]
        Client[autopaho.ConnectionManager]
        Router[messageRouter]
        Coordinator[LifecycleCoordinator]
    end
    
    subgraph Sources [Sources]
        S1[Source sensors]
        S2[Source devices]
    end
    
    subgraph Targets [Targets]
        T1[Target cloud/data]
        T2[Target alerts]
    end
    
    Client -->|subscribe| S1
    Client -->|subscribe| S2
    Client -->|publish| T1
    Client -->|publish| T2
    
    Router -->|dispatch| S1
    Router -->|dispatch| S2
    
    Coordinator -->|manages| S1
    Coordinator -->|manages| S2
    Coordinator -->|manages| T1
    Coordinator -->|manages| T2
```

## Configuration System

The configuration system supports dynamic updates:

```mermaid
flowchart TB
    subgraph config [ConfigSource]
        CS[ConfigSource]
        CW[ConfigWriter]
    end
    
    subgraph bridge [Bridge Runtime]
        CH[ConfigHandler]
        CM[ConnectionManager]
        PM[PipelineManager]
    end
    
    subgraph connection [Connection Layer]
        CONN[Connection]
        LC[LifecycleCoordinator]
        SP[SourceProvider]
        TP[TargetProvider]
    end
    
    subgraph endpoints [Endpoints]
        SRC[Source]
        TGT[Target]
    end
    
    CS -->|ConfigChange| CH
    CH -->|ConnectionChange| CM
    CH -->|SourceChange| PM
    CH -->|TargetChange| PM
    
    CM -->|manages| CONN
    CONN -->|owns| LC
    LC -->|coordinates| SP
    LC -->|coordinates| TP
    SP -->|creates| SRC
    TP -->|creates| TGT
```

## Configuration Change Flow

How configuration changes are processed:

```mermaid
stateDiagram-v2
    [*] --> Watching: Start
    Watching --> ProcessingChange: ConfigChange received
    
    ProcessingChange --> ConnectionChange: Type is connection
    ProcessingChange --> SourceChange: Type is source
    ProcessingChange --> TargetChange: Type is target
    
    ConnectionChange --> CheckReconnect: UpdateSettings
    CheckReconnect --> Drain: RequiresReconnect true
    CheckReconnect --> ApplySettings: RequiresReconnect false
    Drain --> Reconnect
    Reconnect --> RestoreEndpoints
    RestoreEndpoints --> Watching
    ApplySettings --> Watching
    
    SourceChange --> BeginTransaction
    TargetChange --> BeginTransaction
    BeginTransaction --> ScheduleChanges
    ScheduleChanges --> Commit
    Commit --> UpdatePipelines
    UpdatePipelines --> Watching
```

## Lifecycle Transaction

Atomic changes to Sources/Targets on shared connections:

```mermaid
sequenceDiagram
    participant Handler as ConfigHandler
    participant Coord as LifecycleCoordinator
    participant Txn as Transaction
    participant Client as MQTT Client
    
    Handler->>Coord: BeginTransaction
    Coord-->>Handler: txn
    
    Handler->>Txn: AddSource config1
    Handler->>Txn: RemoveSource id2
    Handler->>Txn: UpdateSource id3 config3
    
    Handler->>Txn: Commit
    
    Txn->>Txn: collect topic changes
    Txn->>Client: Unsubscribe removed topics
    Client-->>Txn: ok
    Txn->>Client: Subscribe new topics
    Client-->>Txn: ok
    Txn->>Txn: create/remove Source instances
    Txn-->>Handler: LifecycleChangeResult
```

## Credentials System

The credentials system supports both inline credentials and URI-based resolution:

```mermaid
flowchart TB
    subgraph config [Connection Config]
        Creds[Credentials]
    end
    
    subgraph detection [Detection]
        IsURI{IsServerURI?}
    end
    
    subgraph inline [Inline Path]
        UP[UsernamePassword]
        TLS[TlsCredentials]
    end
    
    subgraph resolution [Resolution Path]
        Resolver[credentials.Resolver]
        Repo[CredentialsRepository]
    end
    
    Creds --> IsURI
    IsURI -->|No| UP
    IsURI -->|No| TLS
    IsURI -->|Yes| Resolver
    Resolver --> Repo
    Repo -->|pms://| PMS[AWS Parameter Store]
    Repo -->|file://| File[File Repository]
```

### Credential Types

- **Inline**: Actual credentials embedded in configuration
  - `UsernamePasswordCredentials`: Username and password
  - `TlsCredentials`: Certificates and keys (PEM encoded)

- **URI-based**: References to external credential stores
  - `pms://tenant/app/creds`: AWS Parameter Store
  - `file://path/to/creds`: Local file system

### ServerURI Detection

A string is considered a serverURI if it matches the pattern `[a-z]+://.*`:

```go
func IsServerURI(s string) bool {
    idx := strings.Index(s, "://")
    if idx <= 0 {
        return false
    }
    scheme := s[:idx]
    for _, c := range scheme {
        if c < 'a' || c > 'z' {
            return false
        }
    }
    return true
}
```

## Registry Interfaces

The system uses optional admin interfaces for CRUD operations:

### ConnectionRegistry (Read-only)

```go
type ConnectionRegistry interface {
    GetConnection(id string) (Connection, error)
    ListConnections() ([]Connection, error)
    CreateConnection(ctx context.Context, config ConnectionConfig) (Connection, error)
}
```

### ConnectionAdminRegistry (Optional CRUD)

```go
type ConnectionAdminRegistry interface {
    ConnectionRegistry
    UpdateConnection(ctx context.Context, id string, settings ConnectionSettingsConfig) error
    DeleteConnection(ctx context.Context, id string) error
    GetOrCreateConnection(ctx context.Context, config ConnectionConfig) (Connection, error)
}
```

### CredentialsRepository (Read-only)

```go
type CredentialsRepository interface {
    GetScheme() string
    GetNamespace() string
    GetCredentials(serverURI string) (*Credentials, error)
}
```

### CredentialsAdminRepository (Optional CRUD)

```go
type CredentialsAdminRepository interface {
    CredentialsRepository
    CreateCredentials(ctx context.Context, serverURI string, creds *Credentials) error
    UpdateCredentials(ctx context.Context, serverURI string, creds *Credentials, version int64) error
    DeleteCredentials(ctx context.Context, serverURI string, version int64) error
    ListCredentials(ctx context.Context, prefix string) ([]string, error)
}
```

## Key Design Principles

1. **Minimal Reconnect**: Connection updates check if reconnect is actually needed
2. **Source/Target as Unit**: Topic changes create/destroy/replace instances
3. **Lifecycle Coordinator**: For shared connections, coordinate changes atomically
4. **Drain-First**: Always drain in-flight messages before disruptive changes
5. **Optional Admin**: Admin interfaces are optional extensions of read-only interfaces
