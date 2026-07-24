// Command agent is the scion-node-agent: it bootstraps SCION endhost
// configuration (topology + TRCs), renders SIG prefix-exchange policies
// from node identity, and runs the embedded SCION-IP gateway.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/mkowalski/scion-k8s-operator/internal/agent/bootstrap"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/config"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/daemonapi"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/health"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/kube"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/policy"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/sig"
)

// daemonAPIAddr is fixed by the spec: the sciond gRPC API is only exposed
// on localhost at the standard sciond port (spec: EnableDaemonAPI serves
// on 127.0.0.1:30255); it is deliberately not configurable.
const daemonAPIAddr = "127.0.0.1:30255"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("agent failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	info, err := nodeInfo(ctx, cfg)
	if err != nil {
		return fmt.Errorf("node identity: %w", err)
	}
	nodeIP := net.ParseIP(info.InternalIP)
	if nodeIP == nil {
		return fmt.Errorf("invalid node IP %q", info.InternalIP)
	}
	log.Info("node identity", "podCIDRs", info.PodCIDRs, "nodeIP", info.InternalIP)

	confDir := filepath.Join(cfg.StateDir, "etc")
	trafficPolicyFile := filepath.Join(cfg.StateDir, "gateway-traffic.json")
	routingPolicyFile := filepath.Join(cfg.StateDir, "gateway-routing.policy")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return err
	}

	disc, err := bootstrap.NewDiscoverer(cfg.BootstrapMode, cfg.DiscoveryURL, cfg.DNSDomain, cfg.DHCPInterface)
	if err != nil {
		return fmt.Errorf("bootstrap discoverer: %w", err)
	}
	h := health.New()
	if err := bootstrap.Fetch(ctx, disc, confDir); err != nil {
		return fmt.Errorf("initial bootstrap fetch: %w", err)
	}
	h.SetReady(health.ComponentBootstrap, true)
	log.Info("bootstrap complete", "dir", confDir)

	in, err := policyInput(confDir, info, cfg)
	if err != nil {
		return err
	}
	if err := renderPolicies(in, trafficPolicyFile, routingPolicyFile); err != nil {
		return fmt.Errorf("initial policy render: %w", err)
	}
	log.Info("policies rendered", "localIA", in.LocalIA, "advertised", in.AdvertisedNets)

	// reload is 1-buffered: the embedded gateway performs its own initial
	// blocking send on this channel to load policies at startup
	// (scion v0.15.0 gateway/gateway.go:367-371 sends ConfigReloadTrigger
	// <- struct{}{} in a goroutine; the config loader is the receiver).
	// We only push (non-blocking) on policy re-render.
	reload := make(chan struct{}, 1)
	topoChanged := make(chan struct{}, 1)

	// The health mux doubles as the gateway's debug mux so status pages
	// are exposed on the metrics port.
	mux := h.Mux()

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return bootstrap.Run(ctx, disc, confDir, cfg.RefreshInterval, topoChanged)
	})

	g.Go(func() error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-topoChanged:
				in, err := policyInput(confDir, info, cfg)
				if err != nil {
					log.Error("policy input after topology change", "err", err)
					continue
				}
				if err := renderPolicies(in, trafficPolicyFile, routingPolicyFile); err != nil {
					log.Error("policy re-render", "err", err)
					continue
				}
				select {
				case reload <- struct{}{}:
				default:
				}
				log.Info("topology changed; policies re-rendered, reload triggered")
			}
		}
	})

	// sigUp is closed once the gateway's standalone daemon connector is
	// constructed. The connector (scionproto pkg/daemon/standalone.go)
	// MustRegisters metrics that daemonapi.Run also registers; daemonapi
	// registers tolerantly, so it must go second — gate it on the gateway.
	sigUp := make(chan struct{})
	g.Go(func() error {
		log.Info("starting gateway", "tun", cfg.TunName)
		defer h.SetReady(health.ComponentGateway, false)
		return sig.Run(ctx, sig.Params{
			ConfigDir:         confDir,
			TrafficPolicyFile: trafficPolicyFile,
			RoutingPolicyFile: routingPolicyFile,
			TunName:           cfg.TunName,
			NodeIP:            nodeIP,
			ReloadTrigger:     reload,
			DebugMux:          mux,
			// Readiness flips only once the gateway is constructed (see
			// sig.Params.OnUp for the remaining optimism caveat).
			OnUp: func() {
				h.SetReady(health.ComponentGateway, true)
				close(sigUp)
			},
		})
	})

	if cfg.EnableDaemonAPI {
		g.Go(func() error {
			select {
			case <-sigUp:
			case <-ctx.Done():
				return ctx.Err()
			}
			log.Info("starting daemon API", "addr", daemonAPIAddr)
			return daemonapi.Run(ctx, confDir, cfg.StateDir, daemonAPIAddr)
		})
	}

	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux}
	g.Go(func() error {
		log.Info("serving metrics/health", "addr", cfg.MetricsAddr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	})

	err = g.Wait()
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("shutdown complete")
	return nil
}

// nodeInfo determines this node's pod CIDRs and IP. If SCION_LOCAL_PREFIXES
// is set (comma-separated CIDRs, plus SCION_NODE_IP), the Kubernetes API is
// skipped entirely so the agent can run outside a cluster.
func nodeInfo(ctx context.Context, cfg config.Config) (kube.NodeInfo, error) {
	if prefixes := os.Getenv("SCION_LOCAL_PREFIXES"); prefixes != "" {
		ip := os.Getenv("SCION_NODE_IP")
		if net.ParseIP(ip) == nil {
			return kube.NodeInfo{}, fmt.Errorf("SCION_NODE_IP required and must be a valid IP when SCION_LOCAL_PREFIXES is set, got %q", ip)
		}
		return kube.NodeInfo{PodCIDRs: split(prefixes), InternalIP: ip}, nil
	}
	rc, err := rest.InClusterConfig()
	if err != nil {
		return kube.NodeInfo{}, fmt.Errorf("in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return kube.NodeInfo{}, err
	}
	return kube.GetNodeInfo(ctx, cs, cfg.NodeName)
}

func policyInput(confDir string, info kube.NodeInfo, cfg config.Config) (policy.Input, error) {
	ia, err := localIA(confDir)
	if err != nil {
		return policy.Input{}, err
	}
	return policy.Input{
		LocalIA:        ia,
		AdvertisedNets: advertisedNets(info, cfg),
		AcceptISDASes:  split(cfg.AcceptISDASes),
		ForbiddenCIDRs: split(cfg.ForbiddenCIDRs),
	}, nil
}

// localIA reads the local ISD-AS from <confDir>/topology.json.
func localIA(confDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(confDir, "topology.json"))
	if err != nil {
		return "", err
	}
	var topo struct {
		ISDAS string `json:"isd_as"`
	}
	if err := json.Unmarshal(data, &topo); err != nil {
		return "", fmt.Errorf("parsing topology.json: %w", err)
	}
	if topo.ISDAS == "" {
		return "", fmt.Errorf("topology.json has no isd_as field")
	}
	return topo.ISDAS, nil
}

// advertisedNets assembles the prefixes this node announces: pod CIDRs
// and/or the node IP as a host route, per config.
func advertisedNets(info kube.NodeInfo, cfg config.Config) []string {
	var nets []string
	if cfg.AdvertisePodCIDR {
		nets = append(nets, info.PodCIDRs...)
	}
	if cfg.AdvertiseNodeIP {
		bits := "/32"
		if ip := net.ParseIP(info.InternalIP); ip != nil && ip.To4() == nil {
			bits = "/128"
		}
		nets = append(nets, info.InternalIP+bits)
	}
	return nets
}

// split splits a comma-separated list, trimming spaces and dropping empties.
func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// renderPolicies renders both policy files. os.WriteFile is not atomic,
// but the gateway only re-reads on reload trigger, which we send after
// both writes complete.
func renderPolicies(in policy.Input, trafficFile, routingFile string) error {
	traffic, err := policy.RenderTrafficPolicy(in)
	if err != nil {
		return err
	}
	routing, err := policy.RenderRoutingPolicy(in)
	if err != nil {
		return err
	}
	if err := os.WriteFile(trafficFile, []byte(traffic), 0o644); err != nil {
		return err
	}
	return os.WriteFile(routingFile, []byte(routing), 0o644)
}
