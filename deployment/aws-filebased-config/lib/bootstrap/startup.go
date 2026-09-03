package bootstrap

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/ports"
)

// App startup: everything between "the process has a bootstrap config" and "the
// process is serving". The order in Start is load-bearing throughout — config
// source, coordinated boot resolution, profile policy, runtime install, cluster
// rollout baseline, then listeners — and each step's comment says what breaks if
// it moves.

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
		// The runtime is installed BEFORE the transport, admin and monitor
		// listeners and before the config watcher, so every failure below leaves
		// it running — sessions connected, stores open, drainers and lease
		// renewals live — with no reference left to stop it. Release it under a
		// bounded, detached context so a retried Start (or a caller falling back
		// to another port) cannot run two runtimes against the same brokers and
		// leases.
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
	if a.dynamoDBClient == nil {
		client, err := newDynamoDBClient(ctx, a.cfg)
		if err != nil {
			return err
		}
		a.dynamoDBClient = client
	}

	source := newOptionalFileSource(a.cfg.ConfigFilePath, a.pluginRegistry, a.logger, func() *ports.BridgeConfig {
		return defaultLogicalConfig(a.cfg)
	})
	watcher := newPollWatcher(ctx, a.cfg, a.pluginRegistry, a.logger)
	a.manager = config.NewManager(
		config.Layer{Name: "file", Loader: source, Watcher: watcher},
		config.WithManagerLogger(a.logger),
	)

	logicalCfg, err := a.manager.Load(ctx)
	if err != nil {
		return err
	}
	a.logicalRef.Set(logicalCfg)

	// Coordinated cluster rollout boot resolution (ADR 0013). ONLY a
	// coordinated boot config wires the barrier host; every other deployment boots
	// exactly as before. The joiner resolves which config this member actually boots
	// on — the durable last-committed config when the boot config is a candidate the
	// barrier has not committed (a restart after an abort, or mid-rollout), else the
	// boot config unchanged — so a restarted member never runs a config no peer runs.
	bootCfg := logicalCfg
	if bridge.IsCoordinatedRollout(logicalCfg) {
		if err := a.buildRolloutDriver(ctx); err != nil {
			return err
		}
		if a.rolloutDriver == nil {
			return fmt.Errorf("bootstrap: cluster.rollout: coordinated requires a restart-stable member_id " +
				"(bootstrap member_id, which must appear in bridge.cluster.members) and a rollout store; " +
				"the barrier is not wired, so a coordinated deployment cannot start. An interchangeable " +
				"autoscaled worker has no such identity and must instead take config changes through " +
				"whole-cohort replacement (see docs/runbooks/cluster-config-rollout.md). Refusing to start")
		}
		resolved, rerr := a.rolloutDriver.ResolveBoot(ctx, logicalCfg)
		if rerr != nil {
			return rerr
		}
		bootCfg = resolved
	}

	// The file-based profile configures the admin/monitor listeners from
	// bootstrap env/SSM (a.cfg + apiKeysRef) and expects TLS to terminate at the
	// load balancer (ALB), so the bridge config `http:` block is not honored.
	// Enforce that policy BEFORE building the runtime or starting any server: a
	// tls_cert_file/tls_key_file pair is an explicit "encrypt this" the profile
	// cannot satisfy, so fail closed rather than silently serve the admin API in
	// plaintext; a bare addrs/keys block is warned and ignored.
	if err := checkIgnoredHTTPBlock(a.logger, bootCfg); err != nil {
		return err
	}

	// One process budget, and it is the one the operator wrote down. Run spends
	// a.shutdownTimeout on the whole SIGTERM path — config watcher, rollout
	// drive, HTTP servers, runtime drain, stores, telemetry — so leaving it on
	// an invisible 30s constant while bridge.shutdown_timeout said something
	// else meant the documented budget governed nothing. An explicit
	// WithShutdownTimeout still wins (library and test callers).
	if !a.shutdownTimeoutPinned {
		if d := bootCfg.Bridge.ShutdownTimeoutDuration(); d > 0 {
			a.shutdownTimeout = d
		}
	}

	if _, err := a.applyLogicalIfChanged(ctx, bootCfg, true); err != nil {
		return err
	}
	a.reconcileBootApplyResult(logicalCfg, bootCfg)

	// Establish the cohort's generation-zero baseline (see seedRolloutBaseline).
	// It runs HERE — after the boot config has been admitted, built and installed,
	// and before any listener is up — so a member only ever publishes a baseline it
	// has itself proven it can run. Seeding earlier would let a config this process
	// then refuses to start on become the artifact every peer recovers to, which no
	// later redeploy could dislodge.
	if err := a.seedRolloutBaseline(ctx, bootCfg); err != nil {
		return err
	}

	// Every node starts the transport, admin, and monitor servers regardless
	// of NodeRole (workers still expose the admin listener today — see
	// infra.BootstrapConfig.NodeRole). NodeRole IS consulted below for the
	// config single-writer posture (apiCfg.ConfigSingleWriter): only the
	// control/single node is the sole durable config writer.
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
			if rt := a.runtimeRef.Get(); rt != nil {
				return rt
			}
			// No active runtime. During a normal swap window this is
			// transient — return nil so /live stays 200 and the process is
			// not restarted. Once WEDGED (a prepare/commit swap AND its
			// recovery both failed) there is no self-recovery path, so
			// expose a sentinel terminal runtime: the monitor /live probe
			// (503 only when rt != nil && rt.Terminal()) then fails closed
			// and the orchestrator restarts the task. This closes the
			// wedged-nil blind spot on the /live side, matching the terminal
			// backstop's coverage via runtimeTerminal().
			if a.wedged.Load() {
				return terminalRuntime{}
			}
			return nil
		},
		ConfigStore: &cfgparser.FileStore{Path: a.cfg.ConfigFilePath, Registry: a.pluginRegistry},
		// ConfigProvider must expose the *effective* (currently running)
		// config, so read from appliedRef -- the config of the last
		// successfully-applied runtime. logicalRef holds the last config
		// read from disk, which may be a reload that FAILED validation or
		// apply (watchLoop keeps the last-good runtime on rejection); using
		// it here would surface a rejected config to operators as if it were
		// live. appliedRef is nil only when nothing is cleanly running, and
		// every configProvider consumer handles nil (GET /config -> 503).
		ConfigProvider: a.appliedRef.Get,
		// Surface both watcher failure and desired/running apply divergence.
		ConfigWatchProvider: a.configWatchHealth,
		// ConfigApplier converges the running runtime in-band when a config
		// is committed through the admin transactions API, reusing the exact
		// reload path the file watcher drives (applyLogicalConfig) instead of
		// waiting for the next poll. httpapi invokes it AFTER the durable
		// write, so a returned error surfaces as committed_not_applied (the
		// operator reconciles) rather than a false "committed" while the
		// runtime diverges. Without this wiring the committed_not_applied /
		// errConfigApplyFailed path is dead in the shipped binary.
		ConfigApplier: a.applyCommittedConfig,
		// ConfigSingleWriter asserts THIS admin process is the sole durable
		// writer of the config store. The profile's ConfigStore is a
		// parser.FileStore (non-CAS: no ports.ConditionalConfigStore), so the
		// httpapi config-transaction commit path FAILS CLOSED on a durable
		// commit unless single-writer is asserted. In the file-based profile
		// the CONTROL/single node owns the RW EFS mount and is the only admin
		// writer (GoBridgeSingle is one task; GoBridgeCluster forces the
		// control service to DesiredCount=1 and mounts workers RO), so the
		// control role IS the sole writer. Derive the flag from NodeRole rather
		// than hardcoding: worker nodes get false (their commits correctly fail
		// closed — a RO EFS mount could not durably persist anyway), and a
		// genuine multi-writer deployment must instead wire a CAS ConfigStore
		// (ports.ConditionalConfigStore.SaveIfVersion — see xcut filestore-cas),
		// which is always safe regardless of this flag.
		ConfigSingleWriter: a.configSingleWriter(),
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

	watchCh, err := a.manager.Watch(ctx)
	if err != nil {
		a.manager.Stop()
		_ = a.httpServer.Stop(context.Background())
		_ = a.transportServer.Stop(context.Background())
		return fmt.Errorf("bootstrap: start config watcher: %w", err)
	}

	watchCtx, watchCancel := context.WithCancel(ctx)
	a.watchCancel = watchCancel
	a.rootCtx = watchCtx
	a.started = true

	// Watch the INITIALLY-applied runtime for convergence too (initial
	// startup has the same truthfulness gap as a reload). installPlan skipped
	// starting it during the pre-rootCtx initial apply, so start it here.
	a.startConvergenceWatch(watchCtx, a.runtimeRef.Get(), a.appliedRef.Get())

	// Start the coordinated cluster rollout drive (ADR 0013): one goroutine
	// per member running the applier every tick and the coordinator half while this
	// member holds the lease. nil when no barrier is wired or the boot config is not
	// coordinated, so this is a no-op for every non-coordinated deployment.
	if a.rolloutDriver != nil {
		a.stopRolloutDrive = a.rolloutDriver.Start(watchCtx, a.clk, a.metricsExporter)
	}

	a.watchWg.Go(func() {
		a.watchLoop(watchCtx, watchCh)
	})
	a.watchWg.Go(func() {
		a.watchTerminal(watchCtx)
	})

	startOK = true
	return nil
}

// configSingleWriter reports whether THIS node is the sole durable writer of
// the config store, which the served httpapi.Config asserts via
// ConfigSingleWriter. The file-based profile's ConfigStore is a
// parser.FileStore — a non-CAS store (it does not implement
// ports.ConditionalConfigStore) — so the httpapi config-transaction commit path
// fails closed on a durable commit unless single-writer is asserted.
//
// The decision is derived from the deploy-time NodeRole rather than hardcoded:
//
//   - control (and the empty default normalized to control by
//     BootstrapConfig.Normalized, covering GoBridgeSingle and library/local
//     use) owns the RW EFS mount and is the ONLY admin writer — GoBridgeCluster
//     forces the control service to DesiredCount=1 — so it is the sole writer.
//   - worker mounts EFS read-only in GoBridgeCluster and is NOT a durable
//     writer; returning false makes a worker's commit fail closed (correct: a
//     RO mount could not persist anyway) instead of a silent last-writer-wins.
//
// A genuine multi-writer deployment (multiple concurrent admin writers against
// one backend) cannot be made safe with a non-CAS FileStore; it MUST wire a
// ports.ConditionalConfigStore instead, which is always safe regardless of this
// flag (xcut filestore-cas: parser.FileStore lacks SaveIfVersion today).
func (a *App) configSingleWriter() bool {
	return a.cfg.NodeRole == deployinfra.NodeRoleControl
}
