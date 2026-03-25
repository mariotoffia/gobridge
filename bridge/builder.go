package bridge

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// Builder constructs a runtime.Runtime from a declarative BridgeConfig.
type Builder struct {
	cfg            *config.BridgeConfig
	transports     map[string]TransportFactory
	storeFactories map[string]StoreFactory
	processors     map[string]ports.Processor
	logger         *slog.Logger
	credStore      ports.CredentialStore
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

// Build validates the configuration, creates all adapters via registered
// factories, and wires them into a runtime.Runtime. The returned runtime
// is not yet started; call Start on it separately. If any step fails,
// previously created sessions are closed to prevent resource leaks.
func (b *Builder) Build(ctx context.Context) (_ *runtime.Runtime, retErr error) {
	if err := config.Validate(b.cfg); err != nil {
		return nil, fmt.Errorf("bridge: config validation: %w", err)
	}

	leaseStore, outboxStore, dlqStore, err := b.buildStores(ctx)
	if err != nil {
		return nil, err
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

	rtOpts := []runtime.Option{
		runtime.WithLeaseStore(leaseStore),
		runtime.WithOutboxStore(outboxStore),
		runtime.WithDLQStore(dlqStore),
	}
	if b.cfg.Bridge.InstanceID != "" {
		rtOpts = append(rtOpts, runtime.WithInstanceID(b.cfg.Bridge.InstanceID))
	}
	if b.logger != nil {
		rtOpts = append(rtOpts, runtime.WithLogger(b.logger))
	}
	rt := runtime.New(rtOpts...)

	registeredSessions := make(map[string]bool)

	for _, routeDef := range b.cfg.Routes {
		recv, ok := receivers[routeDef.ReceiverID]
		if !ok {
			return nil, fmt.Errorf("bridge: route %q: receiver %q not created", routeDef.ID, routeDef.ReceiverID)
		}

		bindings := toBindings(b.cfg, routeDef.Bindings)
		policy := toRoutePolicy(routeDef)
		sessCfg := toSessionConfig(routeDef.Session)

		var routeSession ports.Session
		var routeSender ports.Sender
		var caps []ports.Capability

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
				return nil, fmt.Errorf("bridge: route %q: session sender %q not created", routeDef.ID, routeDef.Session.SenderID)
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
			return nil, fmt.Errorf("bridge: route %q: no sender resolved", routeDef.ID)
		}

		procs, procErr := b.resolveProcessors(routeDef.Processors)
		if procErr != nil {
			return nil, fmt.Errorf("bridge: route %q: %w", routeDef.ID, procErr)
		}

		rcfg := runtime.RouteConfig{
			ID:                 routeDef.ID,
			Policy:             policy,
			Bindings:           bindings,
			Processors:         procs,
			SourceCapabilities: caps,
		}

		if err := rt.AddRoute(rcfg, recv, routeSender, routeSession, sessCfg); err != nil {
			return nil, fmt.Errorf("bridge: add route %q: %w", routeDef.ID, err)
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
				return nil, fmt.Errorf("bridge: route %q: binding %q references unknown session %q", routeDef.ID, bd.ID, bd.SessionID)
			}
			snd, sndOk := senders[bd.SenderID]
			if !sndOk {
				return nil, fmt.Errorf("bridge: route %q: binding %q references unknown sender %q", routeDef.ID, bd.ID, bd.SenderID)
			}
			sc := runtime.DefaultSessionConfig(bd.SessionID, true)
			sc.ConnectAfterLease = true
			if err := rt.RegisterSessionSender(sc, sess, snd); err != nil {
				return nil, fmt.Errorf("bridge: register session sender %q: %w", bd.SessionID, err)
			}
			registeredSessions[bd.SessionID] = true
		}
	}

	return rt, nil
}

func (b *Builder) buildStores(ctx context.Context) (ports.LeaseStore, ports.OutboxStore, ports.DLQStore, error) {
	var leaseStore ports.LeaseStore
	var outboxStore ports.OutboxStore
	var dlqStore ports.DLQStore

	if sc := b.cfg.Stores.Lease; sc != nil {
		sf, ok := b.storeFactories[sc.Type]
		if !ok {
			return nil, nil, nil, fmt.Errorf("bridge: no store factory registered for lease type %q", sc.Type)
		}
		s, err := sf.NewLeaseStore(ctx, *sc)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("bridge: create lease store: %w", err)
		}
		leaseStore = s
	}
	if sc := b.cfg.Stores.Outbox; sc != nil {
		sf, ok := b.storeFactories[sc.Type]
		if !ok {
			return nil, nil, nil, fmt.Errorf("bridge: no store factory registered for outbox type %q", sc.Type)
		}
		s, err := sf.NewOutboxStore(ctx, *sc)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("bridge: create outbox store: %w", err)
		}
		outboxStore = s
	}
	if sc := b.cfg.Stores.DLQ; sc != nil {
		sf, ok := b.storeFactories[sc.Type]
		if !ok {
			return nil, nil, nil, fmt.Errorf("bridge: no store factory registered for dlq type %q", sc.Type)
		}
		s, err := sf.NewDLQStore(ctx, *sc)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("bridge: create dlq store: %w", err)
		}
		dlqStore = s
	}

	return leaseStore, outboxStore, dlqStore, nil
}

func (b *Builder) buildSessions(ctx context.Context) (map[string]ports.Session, error) {
	sessions := make(map[string]ports.Session, len(b.cfg.Sessions))
	for _, sd := range b.cfg.Sessions {
		tf, ok := b.transports[sd.Transport]
		if !ok {
			return nil, fmt.Errorf("bridge: no transport factory registered for %q (session %q)", sd.Transport, sd.ID)
		}
		if sd.Options != nil {
			opts, err := b.resolveCredentials(ctx, sd.Options, fmt.Sprintf("session %q", sd.ID))
			if err != nil {
				return nil, err
			}
			sd.Options = opts
		}
		sess, err := tf.NewSession(ctx, sd)
		if err != nil {
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
