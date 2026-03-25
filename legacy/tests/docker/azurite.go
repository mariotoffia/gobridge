package docker

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// ============================================================================
// Azurite Container (Azure Storage Emulator)
// ============================================================================

// AzuriteContainer represents a running Azurite container emulating Azure Storage.
//
// # Overview
//
// Azurite is the official Azure Storage emulator for local development and testing.
// It provides a local environment that emulates Azure Blob, Queue, and Table Storage.
//
// # Blob Storage Features
//
// Supported:
//   - Containers (create, delete, list, get/set metadata, get properties)
//   - Block blobs (Put Block, Put Block List, Get Block List)
//   - Append blobs (Append Block)
//   - Page blobs (Put Page, Get Page Ranges, partial support)
//   - Blob metadata and properties
//   - Blob snapshots
//   - Blob leasing
//   - Copy blob operations
//   - Access tiers (Hot, Cool, Archive - tier changes are accepted but not enforced)
//   - SAS tokens (account-level and service-level)
//   - Soft delete (partial)
//   - Blob versioning (partial)
//   - Flat-namespace analytics workloads
//
// Not supported:
//   - Azure AD authentication (use connection string or SAS)
//   - Geo-redundant storage
//   - Static website hosting
//   - Immutability policies
//   - Object replication
//
// # Queue Storage Features
//
// Supported:
//   - Queue operations (create, delete, list, get/set metadata)
//   - Message operations (put, get, peek, delete, update, clear)
//   - Message visibility timeout
//   - Message TTL
//   - SAS tokens
//
// Not supported:
//   - Azure AD authentication
//
// # Table Storage Features
//
// Supported:
//   - Table operations (create, delete, list)
//   - Entity operations (insert, update, merge, delete, query)
//   - Batch/transaction operations
//   - OData query support ($filter, $select, $top)
//   - Continuation tokens
//   - SAS tokens
//
// Not supported:
//   - Azure AD authentication
//   - Cross-table transactions
//
// # Default Credentials
//
// Azurite uses well-known development credentials:
//   - Account Name: devstoreaccount1
//   - Account Key: Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==
//
// # Known Differences from Azure Storage
//
//   - Single-node only (no replication)
//   - Access tiers are accepted but data is not moved between tiers
//   - Some advanced features may have limited implementation
//   - Error messages may differ slightly from Azure
type AzuriteContainer struct {
	*Container

	// blobPort is the host port for Blob service.
	blobPort int

	// queuePort is the host port for Queue service.
	queuePort int

	// tablePort is the host port for Table service.
	tablePort int

	// Service enablement flags
	blobEnabled  bool
	queueEnabled bool
	tableEnabled bool
}

// Well-known Azurite development credentials
const (
	AzuriteAccountName = "devstoreaccount1"
	AzuriteAccountKey  = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
)

// Default container ports
const (
	AzuriteBlobPort  = 10000
	AzuriteQueuePort = 10001
	AzuriteTablePort = 10002
)

// BlobPort returns the host port for the Blob service.
// Returns 0 if Blob service is not enabled.
func (a *AzuriteContainer) BlobPort() int {
	if !a.blobEnabled {
		return 0
	}
	return a.blobPort
}

// QueuePort returns the host port for the Queue service.
// Returns 0 if Queue service is not enabled.
func (a *AzuriteContainer) QueuePort() int {
	if !a.queueEnabled {
		return 0
	}
	return a.queuePort
}

// TablePort returns the host port for the Table service.
// Returns 0 if Table service is not enabled.
func (a *AzuriteContainer) TablePort() int {
	if !a.tableEnabled {
		return 0
	}
	return a.tablePort
}

// BlobEndpoint returns the Blob service endpoint URL.
// Returns empty string if Blob service is not enabled.
func (a *AzuriteContainer) BlobEndpoint() string {
	if !a.blobEnabled {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/%s", a.blobPort, AzuriteAccountName)
}

// QueueEndpoint returns the Queue service endpoint URL.
// Returns empty string if Queue service is not enabled.
func (a *AzuriteContainer) QueueEndpoint() string {
	if !a.queueEnabled {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/%s", a.queuePort, AzuriteAccountName)
}

// TableEndpoint returns the Table service endpoint URL.
// Returns empty string if Table service is not enabled.
func (a *AzuriteContainer) TableEndpoint() string {
	if !a.tableEnabled {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/%s", a.tablePort, AzuriteAccountName)
}

// BlobEnabled returns true if Blob service is enabled.
func (a *AzuriteContainer) BlobEnabled() bool {
	return a.blobEnabled
}

// QueueEnabled returns true if Queue service is enabled.
func (a *AzuriteContainer) QueueEnabled() bool {
	return a.queueEnabled
}

// TableEnabled returns true if Table service is enabled.
func (a *AzuriteContainer) TableEnabled() bool {
	return a.tableEnabled
}

// ConnectionString returns the full Azure Storage connection string for all enabled services.
// This connection string can be used directly with the Azure SDK.
func (a *AzuriteContainer) ConnectionString() string {
	cs := fmt.Sprintf(
		"DefaultEndpointsProtocol=http;AccountName=%s;AccountKey=%s;",
		AzuriteAccountName,
		AzuriteAccountKey,
	)

	if a.blobEnabled {
		cs += fmt.Sprintf("BlobEndpoint=http://127.0.0.1:%d/%s;", a.blobPort, AzuriteAccountName)
	}
	if a.queueEnabled {
		cs += fmt.Sprintf("QueueEndpoint=http://127.0.0.1:%d/%s;", a.queuePort, AzuriteAccountName)
	}
	if a.tableEnabled {
		cs += fmt.Sprintf("TableEndpoint=http://127.0.0.1:%d/%s;", a.tablePort, AzuriteAccountName)
	}

	return cs
}

// BlobConnectionString returns a connection string for Blob service only.
// Returns empty string if Blob service is not enabled.
func (a *AzuriteContainer) BlobConnectionString() string {
	if !a.blobEnabled {
		return ""
	}
	return fmt.Sprintf(
		"DefaultEndpointsProtocol=http;AccountName=%s;AccountKey=%s;BlobEndpoint=http://127.0.0.1:%d/%s;",
		AzuriteAccountName,
		AzuriteAccountKey,
		a.blobPort,
		AzuriteAccountName,
	)
}

// QueueConnectionString returns a connection string for Queue service only.
// Returns empty string if Queue service is not enabled.
func (a *AzuriteContainer) QueueConnectionString() string {
	if !a.queueEnabled {
		return ""
	}
	return fmt.Sprintf(
		"DefaultEndpointsProtocol=http;AccountName=%s;AccountKey=%s;QueueEndpoint=http://127.0.0.1:%d/%s;",
		AzuriteAccountName,
		AzuriteAccountKey,
		a.queuePort,
		AzuriteAccountName,
	)
}

// TableConnectionString returns a connection string for Table service only.
// Returns empty string if Table service is not enabled.
func (a *AzuriteContainer) TableConnectionString() string {
	if !a.tableEnabled {
		return ""
	}
	return fmt.Sprintf(
		"DefaultEndpointsProtocol=http;AccountName=%s;AccountKey=%s;TableEndpoint=http://127.0.0.1:%d/%s;",
		AzuriteAccountName,
		AzuriteAccountKey,
		a.tablePort,
		AzuriteAccountName,
	)
}

// ============================================================================
// AzuriteBuilder
// ============================================================================

// AzuriteBuilder configures an Azurite container.
type AzuriteBuilder struct {
	// image is the Azurite Docker image.
	image string

	// name is the container name.
	name string

	// Service ports (0 for random)
	blobPort  int
	queuePort int
	tablePort int

	// Service enablement flags
	blobEnabled  bool
	queueEnabled bool
	tableEnabled bool

	// loose enables loose mode (relaxed API validation).
	loose bool

	// skipApiVersionCheck skips API version validation.
	skipApiVersionCheck bool

	// inMemoryPersistence disables disk persistence (default behavior).
	inMemoryPersistence bool

	// dataDir is the host directory for data persistence.
	dataDir string

	// debug enables debug logging.
	debug bool

	// readyTimeout is how long to wait for Azurite to be ready.
	readyTimeout time.Duration

	// cli is the DockerCLI to use.
	cli *DockerCLI
}

// NewAzurite creates a new AzuriteBuilder with sensible defaults.
// By default, all services (Blob, Queue, Table) are enabled.
func NewAzurite() *AzuriteBuilder {
	return &AzuriteBuilder{
		image:               "mcr.microsoft.com/azure-storage/azurite",
		blobPort:            0, // Random port
		queuePort:           0, // Random port
		tablePort:           0, // Random port
		blobEnabled:         true,
		queueEnabled:        true,
		tableEnabled:        true,
		loose:               false,
		skipApiVersionCheck: false,
		inMemoryPersistence: true,
		debug:               false,
		readyTimeout:        60 * time.Second,
		cli:                 NewDockerCLI(),
	}
}

// Image sets the Azurite Docker image.
func (b *AzuriteBuilder) Image(image string) *AzuriteBuilder {
	b.image = image
	return b
}

// Name sets the container name.
func (b *AzuriteBuilder) Name(name string) *AzuriteBuilder {
	b.name = name
	return b
}

// BlobPort sets the host port for Blob service.
// Use 0 for a random available port.
func (b *AzuriteBuilder) BlobPort(port int) *AzuriteBuilder {
	b.blobPort = port
	return b
}

// QueuePort sets the host port for Queue service.
// Use 0 for a random available port.
func (b *AzuriteBuilder) QueuePort(port int) *AzuriteBuilder {
	b.queuePort = port
	return b
}

// TablePort sets the host port for Table service.
// Use 0 for a random available port.
func (b *AzuriteBuilder) TablePort(port int) *AzuriteBuilder {
	b.tablePort = port
	return b
}

// WithBlob enables only the Blob service.
// Call multiple With* methods to enable multiple services.
func (b *AzuriteBuilder) WithBlob() *AzuriteBuilder {
	b.blobEnabled = true
	b.queueEnabled = false
	b.tableEnabled = false
	return b
}

// WithQueue enables only the Queue service.
// Call multiple With* methods to enable multiple services.
func (b *AzuriteBuilder) WithQueue() *AzuriteBuilder {
	b.blobEnabled = false
	b.queueEnabled = true
	b.tableEnabled = false
	return b
}

// WithTable enables only the Table service.
// Call multiple With* methods to enable multiple services.
func (b *AzuriteBuilder) WithTable() *AzuriteBuilder {
	b.blobEnabled = false
	b.queueEnabled = false
	b.tableEnabled = true
	return b
}

// WithAllServices enables all services (Blob, Queue, Table).
func (b *AzuriteBuilder) WithAllServices() *AzuriteBuilder {
	b.blobEnabled = true
	b.queueEnabled = true
	b.tableEnabled = true
	return b
}

// WithServices enables the specified services.
// Valid service names: "blob", "queue", "table".
func (b *AzuriteBuilder) WithServices(services ...string) *AzuriteBuilder {
	b.blobEnabled = false
	b.queueEnabled = false
	b.tableEnabled = false

	for _, svc := range services {
		switch svc {
		case "blob":
			b.blobEnabled = true
		case "queue":
			b.queueEnabled = true
		case "table":
			b.tableEnabled = true
		}
	}
	return b
}

// EnableBlob enables the Blob service (additive).
func (b *AzuriteBuilder) EnableBlob() *AzuriteBuilder {
	b.blobEnabled = true
	return b
}

// EnableQueue enables the Queue service (additive).
func (b *AzuriteBuilder) EnableQueue() *AzuriteBuilder {
	b.queueEnabled = true
	return b
}

// EnableTable enables the Table service (additive).
func (b *AzuriteBuilder) EnableTable() *AzuriteBuilder {
	b.tableEnabled = true
	return b
}

// DisableBlob disables the Blob service.
func (b *AzuriteBuilder) DisableBlob() *AzuriteBuilder {
	b.blobEnabled = false
	return b
}

// DisableQueue disables the Queue service.
func (b *AzuriteBuilder) DisableQueue() *AzuriteBuilder {
	b.queueEnabled = false
	return b
}

// DisableTable disables the Table service.
func (b *AzuriteBuilder) DisableTable() *AzuriteBuilder {
	b.tableEnabled = false
	return b
}

// Loose enables loose mode which provides relaxed API validation.
// Useful when testing against SDKs that may use slightly different API patterns.
func (b *AzuriteBuilder) Loose(enabled bool) *AzuriteBuilder {
	b.loose = enabled
	return b
}

// SkipApiVersionCheck skips API version validation.
// Useful for compatibility with older SDK versions.
func (b *AzuriteBuilder) SkipApiVersionCheck(skip bool) *AzuriteBuilder {
	b.skipApiVersionCheck = skip
	return b
}

// InMemory enables in-memory persistence (no disk writes).
// This is the default behavior.
func (b *AzuriteBuilder) InMemory(enabled bool) *AzuriteBuilder {
	b.inMemoryPersistence = enabled
	if enabled {
		b.dataDir = ""
	}
	return b
}

// DataDir sets the host directory for data persistence.
// Automatically disables in-memory mode.
func (b *AzuriteBuilder) DataDir(dir string) *AzuriteBuilder {
	b.dataDir = dir
	b.inMemoryPersistence = false
	return b
}

// Debug enables debug logging.
func (b *AzuriteBuilder) Debug(enabled bool) *AzuriteBuilder {
	b.debug = enabled
	return b
}

// ReadyTimeout sets how long to wait for Azurite to be ready.
func (b *AzuriteBuilder) ReadyTimeout(d time.Duration) *AzuriteBuilder {
	b.readyTimeout = d
	return b
}

// WithCLI sets a custom DockerCLI.
func (b *AzuriteBuilder) WithCLI(cli *DockerCLI) *AzuriteBuilder {
	b.cli = cli
	return b
}

// Start creates and starts the Azurite container.
func (b *AzuriteBuilder) Start(ctx context.Context) (*AzuriteContainer, error) {
	// Ensure at least one service is enabled
	if !b.blobEnabled && !b.queueEnabled && !b.tableEnabled {
		return nil, fmt.Errorf("at least one service (blob, queue, or table) must be enabled")
	}

	// Pick free ports if not specified
	blobPort := b.blobPort
	if b.blobEnabled && blobPort == 0 {
		p, err := pickFreePort()
		if err != nil {
			return nil, fmt.Errorf("failed to pick Blob port: %w", err)
		}
		blobPort = p
	}

	queuePort := b.queuePort
	if b.queueEnabled && queuePort == 0 {
		p, err := pickFreePort()
		if err != nil {
			return nil, fmt.Errorf("failed to pick Queue port: %w", err)
		}
		queuePort = p
	}

	tablePort := b.tablePort
	if b.tableEnabled && tablePort == 0 {
		p, err := pickFreePort()
		if err != nil {
			return nil, fmt.Errorf("failed to pick Table port: %w", err)
		}
		tablePort = p
	}

	// Build command arguments
	var cmdArgs []string

	// Add service-specific arguments
	if b.blobEnabled {
		cmdArgs = append(cmdArgs, "azurite-blob", "--blobHost", "0.0.0.0", "--blobPort", "10000")
	}
	if b.queueEnabled {
		if len(cmdArgs) > 0 {
			// If blob is enabled, we need to use the full azurite command
			cmdArgs = []string{"azurite", "--blobHost", "0.0.0.0", "--queueHost", "0.0.0.0"}
		} else {
			cmdArgs = append(cmdArgs, "azurite-queue", "--queueHost", "0.0.0.0", "--queuePort", "10001")
		}
	}
	if b.tableEnabled {
		if len(cmdArgs) > 0 && cmdArgs[0] != "azurite" {
			// Need to switch to full azurite command
			cmdArgs = []string{"azurite", "--blobHost", "0.0.0.0", "--queueHost", "0.0.0.0", "--tableHost", "0.0.0.0"}
		} else if len(cmdArgs) == 0 {
			cmdArgs = append(cmdArgs, "azurite-table", "--tableHost", "0.0.0.0", "--tablePort", "10002")
		} else {
			cmdArgs = append(cmdArgs, "--tableHost", "0.0.0.0")
		}
	}

	// Simplify: if multiple services, just use full azurite command
	enabledCount := 0
	if b.blobEnabled {
		enabledCount++
	}
	if b.queueEnabled {
		enabledCount++
	}
	if b.tableEnabled {
		enabledCount++
	}

	if enabledCount > 1 {
		cmdArgs = []string{"azurite", "--blobHost", "0.0.0.0", "--queueHost", "0.0.0.0", "--tableHost", "0.0.0.0"}
	}

	// Add data location
	if b.dataDir != "" {
		cmdArgs = append(cmdArgs, "--location", "/data")
	} else if b.inMemoryPersistence {
		cmdArgs = append(cmdArgs, "--inMemoryPersistence")
	}

	// Add loose mode
	if b.loose {
		cmdArgs = append(cmdArgs, "--loose")
	}

	// Add skip API version check
	if b.skipApiVersionCheck {
		cmdArgs = append(cmdArgs, "--skipApiVersionCheck")
	}

	// Add debug mode
	if b.debug {
		cmdArgs = append(cmdArgs, "--debug", "/data/debug.log")
	}

	// Build container
	builder := NewContainerBuilder().
		Image(b.image).
		ServiceType("azurite").
		Cmd(cmdArgs...).
		ReadyTimeout(b.readyTimeout).
		WithCLI(b.cli)

	if b.name != "" {
		builder.Name(b.name)
	}

	// Add port mappings
	if b.blobEnabled {
		builder.Port(blobPort, AzuriteBlobPort)
	}
	if b.queueEnabled {
		builder.Port(queuePort, AzuriteQueuePort)
	}
	if b.tableEnabled {
		builder.Port(tablePort, AzuriteTablePort)
	}

	// Add data volume if persistence is enabled
	if b.dataDir != "" {
		builder.Volume(b.dataDir, "/data")
	}

	// Add ready check - verify the Blob service responds (if enabled) or Queue/Table
	builder.ReadyCheck(func(ctx context.Context, c *Container) error {
		var checkPort int
		var checkPath string

		if b.blobEnabled {
			checkPort = blobPort
			checkPath = fmt.Sprintf("http://127.0.0.1:%d/%s?comp=list", checkPort, AzuriteAccountName)
		} else if b.queueEnabled {
			checkPort = queuePort
			checkPath = fmt.Sprintf("http://127.0.0.1:%d/%s?comp=list", checkPort, AzuriteAccountName)
		} else {
			checkPort = tablePort
			checkPath = fmt.Sprintf("http://127.0.0.1:%d/%s/Tables", checkPort, AzuriteAccountName)
		}

		req, err := http.NewRequestWithContext(ctx, "GET", checkPath, nil)
		if err != nil {
			return err
		}

		// Add required headers for Azure Storage
		req.Header.Set("x-ms-version", "2020-10-02")

		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		// Azurite returns 403 (AuthenticationFailed) when running without auth
		// but that means it's up and responding
		if resp.StatusCode == 200 || resp.StatusCode == 403 || resp.StatusCode == 400 {
			return nil
		}

		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	})

	// Start container
	container, err := builder.Start(ctx)
	if err != nil {
		return nil, err
	}

	return &AzuriteContainer{
		Container:    container,
		blobPort:     blobPort,
		queuePort:    queuePort,
		tablePort:    tablePort,
		blobEnabled:  b.blobEnabled,
		queueEnabled: b.queueEnabled,
		tableEnabled: b.tableEnabled,
	}, nil
}

// ============================================================================
// Azurite Test Helpers
// ============================================================================

// DefaultAzuriteConfig returns a default configuration with all services enabled.
func DefaultAzuriteConfig() *AzuriteBuilder {
	return NewAzurite().
		WithAllServices().
		InMemory(true)
}

// AzuriteForBlob returns a configuration with only Blob service enabled.
func AzuriteForBlob() *AzuriteBuilder {
	return NewAzurite().
		WithBlob().
		InMemory(true)
}

// AzuriteForQueue returns a configuration with only Queue service enabled.
func AzuriteForQueue() *AzuriteBuilder {
	return NewAzurite().
		WithQueue().
		InMemory(true)
}

// AzuriteForTable returns a configuration with only Table service enabled.
func AzuriteForTable() *AzuriteBuilder {
	return NewAzurite().
		WithTable().
		InMemory(true)
}

// AzuriteWithPersistence returns a configuration with data persistence enabled.
func AzuriteWithPersistence(dataDir string) *AzuriteBuilder {
	return NewAzurite().
		WithAllServices().
		DataDir(dataDir)
}

// AzuriteLoose returns a configuration with loose mode enabled.
// Useful for compatibility testing with various SDK versions.
func AzuriteLoose() *AzuriteBuilder {
	return NewAzurite().
		WithAllServices().
		InMemory(true).
		Loose(true).
		SkipApiVersionCheck(true)
}
