package server

import (
	"context"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/relay"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/jobaccounting"
	"github.com/maximhq/bifrost/plugins/governance"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/handlers"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
)

// InitializeRelays builds the Relays manager from the config store,
// wires it to the core plugin pipeline (so governance and logging see every call) and to the global
// proxy, and builds the management handler. Without a config store there is nothing to serve.
// vkReloader reloads the keys an endpoint delete or rename touches; nil uses this server, and a
// deployment that wraps virtual-key reloads (cluster broadcast) passes its own, as for the Agent Gateway.
func (s *BifrostHTTPServer) InitializeRelays(ctx context.Context, vkReloader handlers.VirtualKeyReloader) {
	if s.relays != nil || s.Config == nil || s.Config.ConfigStore == nil {
		return
	}
	if vkReloader == nil {
		vkReloader = s
	}
	manager := relay.NewManager(logger)
	if s.Client != nil {
		manager.SetPluginPipeline(
			func() relay.PluginPipeline { return s.Client.AcquirePluginPipeline() },
			func(pipeline relay.PluginPipeline) {
				if p, ok := pipeline.(*bifrost.PluginPipeline); ok {
					s.Client.ReleasePluginPipeline(p)
				}
			},
		)
	}
	manager.SetGlobalProxy(func() *schemas.ProxyConfig { return lib.InheritedGlobalProxy(s.Config.GetGlobalProxyConfig()) })
	cfgs, err := s.Config.ConfigStore.ListRelays(ctx)
	if err != nil {
		logger.Warn("failed to load relays: %v", err)
	} else if err := manager.Replace(cfgs); err != nil {
		logger.Warn("%v", err)
	}
	s.relays = manager
	s.relaysHandler = handlers.NewRelaysHandler(s.Config.ConfigStore, manager, s.Config, vkReloader)
	if len(cfgs) > 0 {
		logger.Info("relays initialized: %d endpoint(s)", len(cfgs))
	}
	// Bootstrap wires the other accounting sweepers before this manager exists, so this one starts
	// here; reloads restart it from WireBatchAccountingSweeper.
	s.startEndpointJobSweeper(s.endpointUsageReporter())
}

// startEndpointJobSweeper (re)starts the sweeper that settles async jobs relay
// routes started. It runs on every reload so the reporter tracks the live governance plugin.
func (s *BifrostHTTPServer) startEndpointJobSweeper(reporter jobaccounting.RelayUsageReporter) {
	s.endpointSweeperMu.Lock()
	defer s.endpointSweeperMu.Unlock()
	if s.endpointSweeperCancel != nil {
		s.endpointSweeperCancel()
		s.endpointSweeperCancel = nil
	}
	if s.relays == nil || s.Config == nil || s.Config.LogsStore == nil {
		return
	}
	store, ok := s.Config.ConfigStore.(jobaccounting.SweepStore)
	if !ok {
		return
	}
	var base context.Context = context.Background()
	if s.Ctx != nil {
		base = s.Ctx
	}
	sweepCtx, cancel := context.WithCancel(base)
	s.endpointSweeperCancel = cancel
	sweeper := jobaccounting.NewRelayJobSweeper(store, s.relays, s.Config.LogsStore, reporter, jobaccounting.RelayJobSweeperConfig{Interval: 30 * time.Second, Logger: logger})
	go sweeper.Run(sweepCtx)
}

// rebuildRelays recompiles every endpoint after the global proxy changes, through the
// handler so the rebuild is serialized with endpoint writes.
func (s *BifrostHTTPServer) rebuildRelays(ctx context.Context) {
	if s.relaysHandler == nil {
		return
	}
	if err := s.relaysHandler.ReloadFromStore(ctx); err != nil {
		logger.Warn("failed to reload relays after a proxy change: %v", err)
	}
}

// endpointUsageReporter returns the live governance plugin as the job settlement reporter, nil when
// governance is not loaded.
func (s *BifrostHTTPServer) endpointUsageReporter() jobaccounting.RelayUsageReporter {
	plugin, err := lib.FindPluginAs[governance.BaseGovernancePlugin](s.Config, s.getGovernancePluginName())
	if err != nil || plugin == nil {
		return nil
	}
	reporter, _ := plugin.(jobaccounting.RelayUsageReporter)
	return reporter
}
