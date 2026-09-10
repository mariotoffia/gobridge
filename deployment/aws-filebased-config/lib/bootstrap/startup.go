package bootstrap

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/ports"
)

// Start establishes the authenticated control plane. Repository observation,
// optional initialization and data-plane activation follow asynchronously; a
// missing document is waiting, never a synthetic empty runtime.

func (a *App) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started {
		return fmt.Errorf("bootstrap: app already started")
	}

	if err := a.cfg.Validate(); err != nil {
		return err
	}

	if a.parameterResolver == nil {
		resolver, err := newSSMParameterResolver(ctx, a.cfg)
		if err != nil {
			return err
		}
		a.parameterResolver = resolver
	}
	// Build the runtime metrics exporter once (noop => nil). It is shared by
	// every bridge.Builder across config reloads and owns a flush goroutine,
	// so it is created here (not in newFactoryRegistry, which runs per
	// reload) and Closed in Stop. On a later Start failure the deferred
	// cleanup below Closes it to avoid a goroutine leak.
	//
	// Built BEFORE the credential store so the store's runtime.Credential
	// resolver can emit credential resolve/stale metrics through this same
	// exporter.
	if a.metricsExporter == nil {
		exporter, err := newMetricsExporter(ctx, a.cfg, a.logger)
		if err != nil {
			return err
		}
		a.metricsExporter = exporter
	}
	startOK := false
	defer func() {
		if startOK {
			return
		}
		// Release any resources owned by an unsuccessful startup attempt.
		if rt := a.runtimeRef.Get(); rt != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(
				context.WithoutCancel(ctx), a.shutdownTimeout)
			if stopErr := stopRuntime(cleanupCtx, rt, a.appliedRef.Get()); stopErr != nil && a.logger != nil {
				a.logger.Warn("bootstrap: stopping the installed runtime after a failed start", "error", stopErr)
			}
			cleanupCancel()
			a.runtimeRef.Set(nil)
			a.appliedRef.Set(nil)
			a.lastAppliedFingerprint = ""
		}
		if a.metricsExporter != nil {
			_ = a.metricsExporter.Close(context.Background())
			a.metricsExporter = nil
		}
	}()

	if a.credentialStore == nil {
		store, err := newDefaultCredentialStore(ctx, a.cfg, a.metricsExporter, a.logger)
		if err != nil {
			return err
		}
		a.credentialStore = store
	}
	if err := a.ensureDynamoDBClient(ctx); err != nil {
		return err
	}

	src, err := a.newConfigSource(ctx)
	if err != nil {
		return err
	}
	a.manager = config.NewManager(src.layer, config.WithManagerLogger(a.logger), config.WithManagerClock(a.clk))

	// Control-plane authentication is bootstrap-owned, not supplied by a runtime.
	admin, monitor, err := resolveControlKeys(ctx, a.parameterResolver, a.cfg)
	if err != nil {
		return err
	}
	a.apiKeysRef.Set(admin, monitor)
	// Every node starts the transport, admin, and monitor servers regardless
	// of NodeRole (workers still expose the admin listener today — see
	// infra.BootstrapConfig.NodeRole). NodeRole IS consulted below for the
	// file config single-writer posture: only control is the sole file writer.
	// A DynamoDB config source uses CAS instead, regardless of role.
	a.transportServer = newTransportServer(a.handlerRef, a.logger)
	if err := a.transportServer.Start(a.cfg.TransportHTTPAddr); err != nil {
		return fmt.Errorf("bootstrap: start transport HTTP server: %w", err)
	}

	apiCfg := httpapi.Config{
		AdminAddr:             a.cfg.AdminAddr,
		MonitorAddr:           a.cfg.MonitorAddr,
		CORSOrigins:           a.cfg.CORSOrigins,
		AdminAPIKeysProvider:  a.apiKeysRef.AdminKeys,
		MonitorAPIKeyProvider: a.apiKeysRef.MonitorKey,
		RuntimeProvider: func() ports.Runtime {
			if a.wedged.Load() {
				return terminalRuntime{}
			}
			if a.activating.Load() || a.missing.Load() {
				return nil
			}
			if rt := a.runtimeRef.Get(); rt != nil {
				return rt
			}
			return nil
		},
		ConfigStore:    src.store,
		ConfigDecoder:  a.decodeInitialConfig,
		ConfigAdmitter: a.admitInitialConfig,
		ConfigReadOnly: a.cfg.NodeRole != "control",
		// ConfigProvider must expose the *effective* (currently running)
		// config, so read from appliedRef -- the config of the last
		// successfully-applied runtime. logicalRef holds the last config
		// read from the source, which may be a reload that FAILED validation or
		// apply (watchLoop keeps the last-good runtime on rejection); using
		// it here would surface a rejected config to operators as if it were
		// live. appliedRef is nil only when nothing is cleanly running, and
		// every configProvider consumer handles nil (GET /config -> 503).
		ConfigProvider:   a.appliedRef.Get,
		TerminalProvider: a.wedged.Load,
		// Surface both watcher failure and desired/running apply divergence.
		ConfigWatchProvider: a.configWatchHealth,
		// ConfigApplier converges the running runtime in-band when a config
		// is committed through the admin transactions API, reusing the exact
		// reload path the config watcher drives (applyLogicalConfig) instead of
		// waiting for the next poll. httpapi invokes it AFTER the durable
		// write, so a returned error surfaces as committed_not_applied (the
		// operator reconciles) rather than a false "committed" while the
		// runtime diverges. Without this wiring the committed_not_applied /
		// errConfigApplyFailed path is dead in the shipped binary.
		ConfigApplier: a.applyCommittedConfig,
		// Only the control node asserts sole-writer authority for a file store.
		// DynamoDB uses ConditionalConfigStore instead, regardless of node role.
		ConfigSingleWriter: src.singleWriter,
	}
	a.httpServer = httpapi.New(nil, apiCfg,
		httpapi.WithServerLogger(a.logger),
		httpapi.WithAuditLogger(httpapi.NewSlogAuditLogger(a.logger)),
		// Reuse the SAME shared, close-shielded exporter the runtime receives
		// (registry.go wires it via bridge.WithMetrics) so admin-plane DLQ
		// redrive metrics land in one sink. nil for the noop profile → no-op.
		httpapi.WithMetrics(a.metricsExporter),
	)
	if err := a.httpServer.Start(ctx); err != nil {
		_ = a.transportServer.Stop(context.Background())
		return fmt.Errorf("bootstrap: start admin/monitor HTTP server: %w", err)
	}

	watchCtx, watchCancel := context.WithCancel(ctx)
	a.watchCancel, a.rootCtx, a.started = watchCancel, watchCtx, true
	a.watchWg.Go(func() { a.observeRepository(watchCtx, src.store) })
	a.watchWg.Go(func() { a.watchTerminal(watchCtx) })
	startOK = true
	return nil
}
