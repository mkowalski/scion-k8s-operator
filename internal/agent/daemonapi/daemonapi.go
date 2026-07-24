// Package daemonapi serves the standard sciond gRPC API (DaemonService)
// for node-local SCION-native applications.
//
// This file is a mechanical port of daemon/cmd/daemon/main.go@v0.15.0
// (github.com/scionproto/scion); update together with dependency bumps.
//
// All scionproto imports (including private/ packages) are intentionally
// isolated in this single file.
//
// Omissions relative to upstream realMain, with rationale:
//   - launcher/env plumbing (launcher.Application, TOML config): we are
//     embedded in the agent and receive configDir/stateDir/listenAddr
//     directly; config defaults are inlined below.
//   - tracing (daemon.InitTracer): not used by the agent.
//   - mgmt API and status pages (mgmtapi, service.StatusPages): not needed;
//     the agent exposes its own health/metrics endpoints.
//   - DRKey (storage.NewDRKeyLevel2Storage, drkey.ClientEngine): disabled.
//     Upstream only wires it when DRKeyLevel2DB.Connection is set; with a
//     nil DRKeyClient the DaemonEngine returns "DRKey is not available"
//     for DRKey RPCs (pkg/daemon/private/engine/engine.go:202).
//   - prometheus HTTP server (Metrics.ServePrometheus): metrics are
//     registered on the default registry and exposed by the agent's
//     health server /metrics endpoint.
//   - hidden path groups (hiddenpath.LoadHiddenPathGroups): not configured;
//     the plain segfetcher gRPC requester is used, matching upstream when
//     no HP groups are configured.
//   - SIGHUP topology reload (app.SIGHUPChannel): the agent rewrites
//     topology via its own mechanisms; no reload channel is wired.
//   - pprof import, chi/cors: mgmt-API only.
package daemonapi

import (
	"context"
	"net"
	"path/filepath"
	"time"

	promgrpc "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/resolver"

	"github.com/scionproto/scion/daemon"
	"github.com/scionproto/scion/pkg/addr"
	"github.com/scionproto/scion/pkg/daemon/fetcher"
	daemontrust "github.com/scionproto/scion/pkg/daemon/private/trust"
	libgrpc "github.com/scionproto/scion/pkg/grpc"
	"github.com/scionproto/scion/pkg/log"
	"github.com/scionproto/scion/pkg/metrics"
	"github.com/scionproto/scion/pkg/private/prom"
	"github.com/scionproto/scion/pkg/private/serrors"
	sdpb "github.com/scionproto/scion/pkg/proto/daemon"
	"github.com/scionproto/scion/private/pathdb"
	"github.com/scionproto/scion/private/periodic"
	"github.com/scionproto/scion/private/revcache"
	segfetchergrpc "github.com/scionproto/scion/private/segment/segfetcher/grpc"
	"github.com/scionproto/scion/private/storage"
	pathstoragemetrics "github.com/scionproto/scion/private/storage/path/metrics"
	truststoragemetrics "github.com/scionproto/scion/private/storage/trust/metrics"
	"github.com/scionproto/scion/private/topology"
	"github.com/scionproto/scion/private/trust"
	"github.com/scionproto/scion/private/trust/compat"
	trustengine "github.com/scionproto/scion/private/trust/config"
	trustmetrics "github.com/scionproto/scion/private/trust/metrics"
)

// queryInterval mirrors daemon/config.DefaultQueryInterval.
const queryInterval = 5 * time.Minute

// Run serves the sciond gRPC API on listenAddr until ctx is done.
// configDir must contain topology.json and a certs/ directory; stateDir
// holds the sqlite databases (sd.path.db, sd.trust.db).
//
// Run registers metrics on the global (default) prometheus registry and
// therefore must be invoked at most once per process.
func Run(ctx context.Context, configDir, stateDir, listenAddr string) (err error) {
	topo, err := topology.NewLoader(topology.LoaderCfg{
		File: filepath.Join(configDir, "topology.json"),
		// Reload omitted: no SIGHUP-driven topology reload in the agent.
		Validator: &topology.DefaultValidator{},
		Metrics:   loaderMetrics(),
	})
	if err != nil {
		return serrors.Wrap("creating topology loader", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, errCtx := errgroup.WithContext(ctx)
	// Ensure goroutines started below are stopped and reaped on every
	// return path (incl. early errors), without masking the first error.
	defer func() {
		cancel()
		if werr := g.Wait(); err == nil {
			err = werr
		}
	}()
	g.Go(func() error {
		defer log.HandlePanic()
		return topo.Run(errCtx)
	})

	// Tracing omitted (upstream: daemon.InitTracer).

	revCache := storage.NewRevocationStorage()
	pathDB, err := storage.NewPathStorage(storage.DBConfig{
		Connection: filepath.Join(stateDir, "sd.path.db"),
	})
	if err != nil {
		return serrors.Wrap("initializing path storage", err)
	}
	pathDB = pathstoragemetrics.WrapDB(pathDB, pathstoragemetrics.Config{
		Driver: string(storage.BackendSqlite),
	})
	defer pathDB.Close()
	defer revCache.Close()
	//nolint:staticcheck // SA1019: mirrors upstream (scionproto/scion#4776).
	cleaner := periodic.Start(pathdb.NewCleaner(pathDB, "sd_segments"),
		300*time.Second, 295*time.Second)
	defer cleaner.Stop()
	//nolint:staticcheck // SA1019: mirrors upstream (scionproto/scion#4776).
	rcCleaner := periodic.Start(revcache.NewCleaner(revCache, "sd_revocation"),
		10*time.Second, 10*time.Second)
	defer rcCleaner.Stop()

	dialer := &libgrpc.TCPDialer{
		SvcResolver: func(dst addr.SVC) []resolver.Address {
			if base := dst.Base(); base != addr.SvcCS {
				panic("unsupported address type, possible implementation error: " +
					base.String())
			}
			targets := []resolver.Address{}
			for _, entry := range topo.ControlServiceAddresses() {
				targets = append(targets, resolver.Address{Addr: entry.String()})
			}
			return targets
		},
	}

	trustDB, err := storage.NewTrustStorage(storage.DBConfig{
		Connection: filepath.Join(stateDir, "sd.trust.db"),
	})
	if err != nil {
		return serrors.Wrap("initializing trust database", err)
	}
	defer trustDB.Close()
	trustDB = truststoragemetrics.WrapDB(trustDB, truststoragemetrics.Config{
		Driver: string(storage.BackendSqlite),
		QueriesTotal: metrics.NewPromCounterFrom(
			prometheus.CounterOpts{
				Name: "trustengine_db_queries_total",
				Help: "Total queries to the database",
			},
			[]string{"driver", "operation", prom.LabelResult},
		),
	})
	certsDir := filepath.Join(configDir, "certs")
	engine, err := daemontrust.NewEngine(
		errCtx, certsDir, topo.IA(), trustDB, dialer,
	)
	if err != nil {
		return serrors.Wrap("creating trust engine", err)
	}
	// Trust engine cache config: upstream defaults (1 minute expiration).
	trustEngineCfg := trustengine.Config{}
	trustEngineCfg.InitDefaults()
	engine.Inspector = trust.CachingInspector{
		Inspector:          engine.Inspector,
		Cache:              trustEngineCfg.Cache.New(),
		CacheHits:          metrics.NewPromCounter(trustmetrics.CacheHitsTotal),
		MaxCacheExpiration: trustEngineCfg.Cache.Expiration.Duration,
	}
	trcLoader := trust.TRCLoader{
		Dir: certsDir,
		DB:  trustDB,
	}
	//nolint:staticcheck // SA1019: mirrors upstream (scionproto/scion#4776).
	trcLoaderTask := periodic.Start(periodic.Func{
		Task: func(ctx context.Context) {
			res, err := trcLoader.Load(ctx)
			if err != nil {
				log.SafeInfo(log.FromCtx(ctx), "TRC loading failed", "err", err)
			}
			if len(res.Loaded) > 0 {
				log.SafeInfo(log.FromCtx(ctx), "Loaded TRCs from disk", "trcs", res.Loaded)
			}
		},
		TaskName: "daemon_trc_loader",
	}, 10*time.Second, 10*time.Second)
	defer trcLoaderTask.Stop()

	// DRKey omitted: upstream only enables it when DRKeyLevel2DB.Connection
	// is configured; a nil DRKeyClient makes DRKey RPCs return
	// "DRKey is not available".

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return serrors.Wrap("listening", err)
	}

	// Hidden path groups omitted: plain segfetcher requester, as upstream
	// uses when no HP groups are configured.
	requester := &segfetchergrpc.Requester{
		Dialer: dialer,
	}

	// Segment verification is always enabled (upstream default when
	// DisableSegVerification is false).
	verifier := compat.Verifier{Verifier: trust.Verifier{
		Engine:             engine,
		Cache:              trustEngineCfg.Cache.New(),
		CacheHits:          metrics.NewPromCounter(trustmetrics.CacheHitsTotal),
		MaxCacheExpiration: trustEngineCfg.Cache.Expiration.Duration,
	}}

	server := grpc.NewServer(
		libgrpc.UnaryServerInterceptor(),
		libgrpc.DefaultMaxConcurrentStreams(),
	)
	sdpb.RegisterDaemonServiceServer(server, daemon.NewServer(
		daemon.ServerConfig{
			IA:          topo.IA(),
			MTU:         topo.MTU(),
			LocalASInfo: topo,
			Fetcher: fetcher.NewFetcher(
				fetcher.FetcherConfig{
					IA:            topo.IA(),
					MTU:           topo.MTU(),
					Core:          topo.Core(),
					NextHopper:    topo,
					RPC:           requester,
					PathDB:        pathDB,
					Inspector:     engine,
					Verifier:      verifier,
					RevCache:      revCache,
					QueryInterval: queryInterval,
				},
			),
			Engine:   engine,
			RevCache: revCache,
			// DRKeyClient nil: DRKey disabled (see omission note above).
			DRKeyClient: nil,
		},
	))

	promgrpc.Register(server)

	g.Go(func() error {
		defer log.HandlePanic()
		if err := server.Serve(listener); err != nil {
			return serrors.Wrap("serving gRPC API", err, "addr", listenAddr)
		}
		return nil
	})

	// mgmt API, status pages, and prometheus HTTP server omitted (see
	// package comment).

	g.Go(func() error {
		defer log.HandlePanic()
		<-errCtx.Done()
		server.GracefulStop()
		return nil
	})

	return g.Wait()
}

// loaderMetrics is ported verbatim from upstream main.go.
func loaderMetrics() topology.LoaderMetrics {
	updates := prom.NewCounterVec("", "",
		"topology_updates_total",
		"The total number of updates.",
		[]string{prom.LabelResult},
	)
	return topology.LoaderMetrics{
		ValidationErrors: metrics.NewPromCounter(updates).With(prom.LabelResult, "err_validate"),
		ReadErrors:       metrics.NewPromCounter(updates).With(prom.LabelResult, "err_read"),
		LastUpdate: metrics.NewPromGauge(
			prom.NewGaugeVec("", "",
				"topology_last_update_time",
				"Timestamp of the last successful update.",
				[]string{},
			),
		),
		Updates: metrics.NewPromCounter(updates).With(prom.LabelResult, prom.Success),
	}
}
