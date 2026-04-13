package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// Builder constructs a runtime.Runtime from a declarative BridgeConfig.
type Builder struct {
	cfg              *config.BridgeConfig
	transports       map[string]TransportFactory
	storeFactories   map[string]StoreFactory
	processors       map[string]ports.Processor
	logger           *slog.Logger
	credStore        ports.CredentialStore
	endpointResolver ports.EndpointResolver
}

// BuilderOption configures a Builder.
type BuilderOption func(*Builder)

// WithLogger sets the logger for the builder and the resulting runtime.
func WithLogger(l *slog.Logger) BuilderOption {
	return func(b *Builder) { b.logger = l }
}

// WithCredentialStore sets the credential store used to resolve
// credentials_uri references in session, receiver, and sender options.
func WithCredentialStore(cs ports.CredentialStore) BuilderOption {
	return func(b *Builder) { b.credStore = cs }
}

// NewBuilder creates a builder from the given configuration.
func NewBuilder(cfg *config.BridgeConfig, opts ...BuilderOption) *Builder {
	b := &Builder{
		cfg:            cfg,
		transports:     make(map[string]TransportFactory),
		storeFactories: make(map[string]StoreFactory),
		processors:     make(map[string]ports.Processor),
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// RegisterTransport registers a transport factory under the given name
// (e.g. "mqtt", "sqs"). Returns the builder for chaining.
func (b *Builder) RegisterTransport(name string, factory TransportFactory) *Builder {
	b.transports[name] = factory
	return b
}

// RegisterStoreFactory registers a store factory under the given name
// (e.g. "dynamodb", "memory", "sqlite"). Returns the builder for chaining.
func (b *Builder) RegisterStoreFactory(name string, factory StoreFactory) *Builder {
	b.storeFactories[name] = factory
	return b
}

// RegisterProcessor registers a named processor that can be referenced
// from route definitions. Returns the builder for chaining.
func (b *Builder) RegisterProcessor(name string, proc ports.Processor) *Builder {
	b.processors[name] = proc
	return b
}

// RegisterEndpointResolver sets a custom EndpointResolver for cluster
// endpoint discovery. When not set, the builder auto-detects the resolver
// based on the runtime environment.
func (b *Builder) RegisterEndpointResolver(r ports.EndpointResolver) *Builder {
	b.endpointResolver = r
	return b
}

// TransportHandlers returns HTTP handlers from transport factories that
// implement HTTPMountable. The map keys are transport names.
func (b *Builder) TransportHandlers() map[string]HTTPMountable {
	handlers := make(map[string]HTTPMountable)
	for name, tf := range b.transports {
		if m, ok := tf.(HTTPMountable); ok {
			handlers[name] = m
		}
	}
	return handlers
}

// PreparedBuild holds pre-validated state from the prepare phase.
// No transport sessions, receivers, or senders have been created yet,
// making it safe to call Prepare while an old runtime still holds
// exclusive transport connections.
type PreparedBuild struct {
	cfg    *config.BridgeConfig
	stores *storeResult
	rtOpts []runtime.Option
}

// Prepare validates config and builds stores but does NOT create
// transport sessions, receivers, or senders. This is the first phase
// of the two-phase build used by the Supervisor in PrepareCommit mode.
func (b *Builder) Prepare(ctx context.Context) (*PreparedBuild, error) {
	if err := config.Validate(b.cfg); err != nil {
		return nil, fmt.Errorf("bridge: config validation: %w", err)
	}

	stores, err := b.buildStores(ctx)
	if err != nil {
		return nil, err
	}

	rtOpts := []runtime.Option{
		runtime.WithLeaseStore(stores.lease),
		runtime.WithOutboxStore(stores.outbox),
		runtime.WithDLQStore(stores.dlq),
	}
	if b.cfg.Bridge.InstanceID != "" {
		rtOpts = append(rtOpts, runtime.WithInstanceID(b.cfg.Bridge.InstanceID))
	}
	if b.logger != nil {
		rtOpts = append(rtOpts, runtime.WithLogger(b.logger))
	}

	endpoints := b.resolveClusterEndpoints(ctx)
	if len(endpoints) > 0 {
		rtOpts = append(rtOpts, runtime.WithClusterEndpoints(endpoints))
	}

	return &PreparedBuild{
		cfg:    b.cfg,
		stores: stores,
		rtOpts: rtOpts,
	}, nil
}

// Complete creates sessions, receivers, senders, wires routes, and
// returns a ready-to-start Runtime. Call after the old runtime has
// released exclusive resources (e.g. MQTT client-ids). If prep is
// nil, Complete returns an error.
func (b *Builder) Complete(ctx context.Context, prep *PreparedBuild) (_ *runtime.Runtime, retErr error) {
	if prep == nil {
		return nil, fmt.Errorf("bridge: Complete called with nil PreparedBuild")
	}

	sessions, err := b.buildSessions(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			for id, s := range sessions {
				if closeErr := s.Close(ctx); closeErr != nil && b.logger != nil {
					b.logger.Warn("closing session after build failure", "session", id, "error", closeErr)
				}
			}
		}
	}()

	receivers, err := b.buildReceivers(ctx, sessions)
	if err != nil {
		return nil, err
	}

	senders, err := b.buildSenders(ctx, sessions)
	if err != nil {
		return nil, err
	}

	rt := runtime.New(prep.rtOpts...)

	if err := b.wireRoutes(rt, sessions, receivers, senders); err != nil {
		return nil, err
	}

	return rt, nil
}

// Build validates the configuration, creates all adapters via registered
// factories, and wires them into a runtime.Runtime. The returned runtime
// is not yet started; call Start on it separately. If any step fails,
// previously created sessions are closed to prevent resource leaks.
func (b *Builder) Build(ctx context.Context) (*runtime.Runtime, error) {
	prep, err := b.Prepare(ctx)
	if err != nil {
		return nil, err
	}
	return b.Complete(ctx, prep)
}

func (b *Builder) wireRoutes(
	rt *runtime.Runtime,
	sessions map[string]ports.Session,
	receivers map[string]ports.Receiver,
	senders map[string]ports.Sender,
) error {
	registeredSessions := make(map[string]bool)

	for _, routeDef := range b.cfg.Routes {
		recv, ok := receivers[routeDef.ReceiverID]
		if !ok {
			return fmt.Errorf("bridge: route %q: receiver %q not created", routeDef.ID, routeDef.ReceiverID)
		}

		bindings := toBindings(b.cfg, routeDef.Bindings)
		policy, policyErr := toRoutePolicyE(routeDef)
		if policyErr != nil {
			return fmt.Errorf("bridge: route %q: %w", routeDef.ID, policyErr)
		}
		sessCfg, sessCfgErr := toSessionConfigE(routeDef.Session)
		if sessCfgErr != nil {
			return fmt.Errorf("bridge: route %q: %w", routeDef.ID, sessCfgErr)
		}

		var routeSession ports.Session
		var routeSender ports.Sender
		var caps []ports.Capability
		var sourceVisTimeout time.Duration

		recvDef := findReceiver(b.cfg, routeDef.ReceiverID)
		if recvDef != nil {
			transport := recvDef.Transport
			if transport == "" {
				if sd := findSession(b.cfg, recvDef.SessionID); sd != nil {
					transport = sd.Transport
				}
			}
			if tf, ok := b.transports[transport]; ok {
				caps = tf.Capabilities()
				if vtp, ok := tf.(VisibilityTimeoutProvider); ok {
					sourceVisTimeout = vtp.VisibilityTimeout()
				}
			}
		}

		if routeDef.Session != nil {
			sid := routeDef.Session.SessionID
			if s, ok := sessions[sid]; ok {
				routeSession = s
			}
			if snd, ok := senders[routeDef.Session.SenderID]; ok {
				routeSender = snd
			} else {
				return fmt.Errorf("bridge: route %q: session sender %q not created", routeDef.ID, routeDef.Session.SenderID)
			}
		} else if len(bindings) > 0 {
			firstBind := bindings[0]
			if snd, ok := senders[firstBind.SenderID]; ok {
				routeSender = snd
			}
			if firstBind.SessionID != "" {
				if s, ok := sessions[firstBind.SessionID]; ok {
					routeSession = s
				}
			}
		}

		if routeSender == nil {
			return fmt.Errorf("bridge: route %q: no sender resolved", routeDef.ID)
		}

		procs, procErr := b.resolveProcessors(routeDef.Processors)
		if procErr != nil {
			return fmt.Errorf("bridge: route %q: %w", routeDef.ID, procErr)
		}

		rcfg := runtime.RouteConfig{
			ID:                      routeDef.ID,
			Policy:                  policy,
			Bindings:                bindings,
			Processors:              procs,
			SourceCapabilities:      caps,
			SourceVisibilityTimeout: sourceVisTimeout,
		}

		// Build content-based resolver from config if present.
		if routeDef.Resolver != nil {
			resolver, resolverErr := buildResolver(routeDef.Resolver, bindings)
			if resolverErr != nil {
				return fmt.Errorf("bridge: route %q: resolver: %w", routeDef.ID, resolverErr)
			}
			rcfg.Resolver = resolver
		}

		// Build per-binding sender registry for DirectHold multi-sender dispatch.
		if len(bindings) > 1 {
			senderReg := make(map[string]ports.Sender, len(bindings))
			for _, bd := range bindings {
				snd, ok := senders[bd.SenderID]
				if !ok {
					return fmt.Errorf("bridge: route %q: binding %q references unknown sender %q",
						routeDef.ID, bd.ID, bd.SenderID)
				}
				senderReg[bd.ID] = snd
			}
			rcfg.Senders = senderReg
		}

		if err := rt.AddRoute(rcfg, recv, routeSender, routeSession, sessCfg); err != nil {
			return fmt.Errorf("bridge: add route %q: %w", routeDef.ID, err)
		}

		if routeDef.Session != nil {
			registeredSessions[routeDef.Session.SessionID] = true
		}

		for _, bd := range bindings {
			if bd.SessionID == "" || registeredSessions[bd.SessionID] {
				continue
			}
			sess, sessOk := sessions[bd.SessionID]
			if !sessOk {
				return fmt.Errorf("bridge: route %q: binding %q references unknown session %q", routeDef.ID, bd.ID, bd.SessionID)
			}
			snd, sndOk := senders[bd.SenderID]
			if !sndOk {
				return fmt.Errorf("bridge: route %q: binding %q references unknown sender %q", routeDef.ID, bd.ID, bd.SenderID)
			}
			sc := runtime.DefaultSessionConfig(bd.SessionID, true)
			sc.ConnectAfterLease = true
			if err := rt.RegisterSessionSender(sc, sess, snd); err != nil {
				return fmt.Errorf("bridge: register session sender %q: %w", bd.SessionID, err)
			}
			registeredSessions[bd.SessionID] = true
		}
	}

	return nil
}

type storeResult struct {
	lease   ports.LeaseStore
	outbox  ports.OutboxStore
	dlq     ports.DLQStore
	leaseDist  bool
	outboxDist bool
	dlqDist    bool
}

func isDistributedFactory(sf StoreFactory) bool {
	if df, ok := sf.(DistributedStoreFactory); ok {
		return df.IsDistributed()
	}
	return false
}

func (b *Builder) buildStores(ctx context.Context) (*storeResult, error) {
	res := &storeResult{}

	if sc := b.cfg.Stores.Lease; sc != nil {
		sf, ok := b.storeFactories[sc.Type]
		if !ok {
			return nil, fmt.Errorf("bridge: no store factory registered for lease type %q", sc.Type)
		}
		s, err := sf.NewLeaseStore(ctx, *sc)
		if err != nil {
			return nil, fmt.Errorf("bridge: create lease store: %w", err)
		}
		if s == nil {
			return nil, fmt.Errorf("bridge: store factory %q returned nil lease store without error", sc.Type)
		}
		res.lease = s
		res.leaseDist = isDistributedFactory(sf)
	}
	if sc := b.cfg.Stores.Outbox; sc != nil {
		sf, ok := b.storeFactories[sc.Type]
		if !ok {
			return nil, fmt.Errorf("bridge: no store factory registered for outbox type %q", sc.Type)
		}
		if err := b.injectStaleClaimDuration(sc); err != nil {
			return nil, err
		}
		s, err := sf.NewOutboxStore(ctx, *sc)
		if err != nil {
			return nil, fmt.Errorf("bridge: create outbox store: %w", err)
		}
		if s == nil {
			return nil, fmt.Errorf("bridge: store factory %q returned nil outbox store without error", sc.Type)
		}
		res.outbox = s
		res.outboxDist = isDistributedFactory(sf)
	}
	if sc := b.cfg.Stores.DLQ; sc != nil {
		sf, ok := b.storeFactories[sc.Type]
		if !ok {
			return nil, fmt.Errorf("bridge: no store factory registered for dlq type %q", sc.Type)
		}
		s, err := sf.NewDLQStore(ctx, *sc)
		if err != nil {
			return nil, fmt.Errorf("bridge: create dlq store: %w", err)
		}
		if s == nil {
			return nil, fmt.Errorf("bridge: store factory %q returned nil dlq store without error", sc.Type)
		}
		res.dlq = s
		res.dlqDist = isDistributedFactory(sf)
	}

	if b.cfg.Bridge.DeploymentMode == "clustered" {
		if res.lease != nil && !res.leaseDist {
			return nil, fmt.Errorf("bridge: clustered deployment requires a distributed LeaseStore; the configured store is process-local")
		}
		if res.outbox != nil && !res.outboxDist {
			return nil, fmt.Errorf("bridge: clustered deployment requires a distributed OutboxStore; the configured store is process-local")
		}
		if res.dlq != nil && !res.dlqDist {
			return nil, fmt.Errorf("bridge: clustered deployment requires a distributed DLQStore; the configured store is process-local")
		}
	}

	return res, nil
}

func (b *Builder) buildSessions(ctx context.Context) (map[string]ports.Session, error) {
	sessions := make(map[string]ports.Session, len(b.cfg.Sessions))

	cleanup := func(exclude string) {
		for id, s := range sessions {
			if id == exclude {
				continue
			}
			if closeErr := s.Close(ctx); closeErr != nil && b.logger != nil {
				b.logger.Warn("closing session after partial failure", "session", id, "error", closeErr)
			}
		}
	}

	for _, sd := range b.cfg.Sessions {
		tf, ok := b.transports[sd.Transport]
		if !ok {
			cleanup("")
			return nil, fmt.Errorf("bridge: no transport factory registered for %q (session %q)", sd.Transport, sd.ID)
		}
		if sd.Options != nil {
			opts, err := b.resolveCredentials(ctx, sd.Options, fmt.Sprintf("session %q", sd.ID))
			if err != nil {
				cleanup("")
				return nil, err
			}
			sd.Options = opts
		}
		sess, err := tf.NewSession(ctx, sd)
		if err != nil {
			cleanup("")
			return nil, fmt.Errorf("bridge: create session %q: %w", sd.ID, err)
		}
		if sess != nil {
			sessions[sd.ID] = sess
		}
	}
	return sessions, nil
}

func (b *Builder) buildReceivers(ctx context.Context, sessions map[string]ports.Session) (map[string]ports.Receiver, error) {
	receivers := make(map[string]ports.Receiver, len(b.cfg.Receivers))
	for _, rd := range b.cfg.Receivers {
		transport := rd.Transport
		if transport == "" {
			if sd := findSession(b.cfg, rd.SessionID); sd != nil {
				transport = sd.Transport
			}
		}
		tf, ok := b.transports[transport]
		if !ok {
			return nil, fmt.Errorf("bridge: no transport factory for %q (receiver %q)", transport, rd.ID)
		}
		var sess ports.Session
		if rd.SessionID != "" {
			sess = sessions[rd.SessionID]
			if sess == nil {
				return nil, fmt.Errorf("bridge: receiver %q references unknown session %q", rd.ID, rd.SessionID)
			}
		}
		if rd.Options != nil {
			opts, err := b.resolveCredentials(ctx, rd.Options, fmt.Sprintf("receiver %q", rd.ID))
			if err != nil {
				return nil, err
			}
			rd.Options = opts
		}
		recv, err := tf.NewReceiver(ctx, rd, sess)
		if err != nil {
			return nil, fmt.Errorf("bridge: create receiver %q: %w", rd.ID, err)
		}
		receivers[rd.ID] = recv
	}
	return receivers, nil
}

func (b *Builder) buildSenders(ctx context.Context, sessions map[string]ports.Session) (map[string]ports.Sender, error) {
	senders := make(map[string]ports.Sender, len(b.cfg.Senders))
	for _, sd := range b.cfg.Senders {
		transport := sd.Transport
		if transport == "" {
			if sess := findSession(b.cfg, sd.SessionID); sess != nil {
				transport = sess.Transport
			}
		}
		tf, ok := b.transports[transport]
		if !ok {
			return nil, fmt.Errorf("bridge: no transport factory for %q (sender %q)", transport, sd.ID)
		}
		var sess ports.Session
		if sd.SessionID != "" {
			sess = sessions[sd.SessionID]
			if sess == nil {
				return nil, fmt.Errorf("bridge: sender %q references unknown session %q", sd.ID, sd.SessionID)
			}
		}
		if sd.Options != nil {
			opts, err := b.resolveCredentials(ctx, sd.Options, fmt.Sprintf("sender %q", sd.ID))
			if err != nil {
				return nil, err
			}
			sd.Options = opts
		}
		snd, err := tf.NewSender(ctx, sd, sess)
		if err != nil {
			return nil, fmt.Errorf("bridge: create sender %q: %w", sd.ID, err)
		}
		senders[sd.ID] = snd
	}
	return senders, nil
}

func (b *Builder) resolveProcessors(names []string) ([]ports.Processor, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]ports.Processor, 0, len(names))
	for _, n := range names {
		p, ok := b.processors[n]
		if !ok {
			return nil, fmt.Errorf("bridge: unknown processor %q", n)
		}
		out = append(out, p)
	}
	return out, nil
}

func (b *Builder) resolveCredentials(ctx context.Context, opts map[string]any, label string) (map[string]any, error) {
	uriVal, hasURI := opts["credentials_uri"]
	if !hasURI {
		return opts, nil
	}

	uri, ok := uriVal.(string)
	if !ok {
		return nil, fmt.Errorf("bridge: %s: credentials_uri must be a string", label)
	}

	if b.credStore == nil {
		return nil, fmt.Errorf("bridge: %s: credentials_uri specified but no credential store registered", label)
	}

	creds, err := b.credStore.Resolve(ctx, uri)
	if err != nil {
		return nil, fmt.Errorf("bridge: %s: resolve credentials: %w", label, err)
	}

	resolved := make(map[string]any, len(opts))
	for k, v := range opts {
		resolved[k] = v
	}
	delete(resolved, "credentials_uri")

	if creds.Password != nil {
		if _, exists := resolved["username"]; !exists {
			resolved["username"] = creds.Password.Username
		}
		if _, exists := resolved["password"]; !exists {
			resolved["password"] = creds.Password.Password
		}
	}
	if creds.TLS != nil {
		if _, exists := resolved["tls_cert"]; !exists {
			resolved["tls_cert"] = creds.TLS.CertPEM
		}
		if _, exists := resolved["tls_key"]; !exists {
			resolved["tls_key"] = creds.TLS.KeyPEM
		}
		if _, exists := resolved["tls_ca"]; !exists && len(creds.TLS.CAPEMs) > 0 {
			resolved["tls_ca"] = creds.TLS.CAPEMs
		}
		if _, exists := resolved["tls_insecure"]; !exists && creds.TLS.InsecureSkipVerify {
			resolved["tls_insecure"] = true
		}
	}

	return resolved, nil
}

func (b *Builder) resolveClusterEndpoints(ctx context.Context) map[string]string {
	if b.cfg.Bridge.Cluster != nil && len(b.cfg.Bridge.Cluster.Endpoints) > 0 {
		return b.cfg.Bridge.Cluster.Endpoints
	}

	if b.endpointResolver != nil {
		listenAddr := ":8080"
		if b.cfg.HTTP != nil && b.cfg.HTTP.AdminAddr != "" {
			listenAddr = b.cfg.HTTP.AdminAddr
		}
		endpoints, err := b.endpointResolver.Resolve(ctx, listenAddr)
		if err != nil {
			if b.logger != nil {
				b.logger.Warn("endpoint resolution failed, cluster routing may be unavailable", "error", err)
			}
			return nil
		}
		return endpoints
	}

	return nil
}

// staleClaimBuffer is the safety margin added on top of StepDownGrace
// when deriving the outbox stale claim duration.
const staleClaimBuffer = 15 * time.Second

// injectStaleClaimDuration derives stale_claim_duration from the
// session StepDownGrace values across all routes and injects it into
// the outbox store config options. This keeps the outbox reclaim
// timeout aligned with the lease lifecycle rather than being an
// independent hardcoded value. The derivation is skipped when the
// user has explicitly set stale_claim_duration in YAML.
//
// The method works on a shallow copy of sc.Options so that the
// original config is not mutated, allowing safe re-derivation on
// subsequent Build() calls with the same config.
func (b *Builder) injectStaleClaimDuration(sc *config.StoreConfig) error {
	if sc.Options != nil {
		if _, explicit := sc.Options["stale_claim_duration"]; explicit {
			return nil
		}
	}

	maxGrace := runtime.DefaultSessionConfig("", true).StepDownGrace
	for _, r := range b.cfg.Routes {
		if r.Session == nil {
			continue
		}
		sessCfg, err := toSessionConfigE(r.Session)
		if err != nil {
			return fmt.Errorf("bridge: route %q: %w", r.ID, err)
		}
		if sessCfg != nil && sessCfg.StepDownGrace > maxGrace {
			maxGrace = sessCfg.StepDownGrace
		}
	}

	opts := make(map[string]any, len(sc.Options)+1)
	for k, v := range sc.Options {
		opts[k] = v
	}
	opts["stale_claim_duration"] = maxGrace + staleClaimBuffer
	sc.Options = opts
	return nil
}
