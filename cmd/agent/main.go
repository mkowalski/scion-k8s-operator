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
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	scionlog "github.com/scionproto/scion/pkg/log"

	"github.com/mkowalski/scion-k8s-operator/internal/agent/bootstrap"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/config"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/daemonapi"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/health"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/kube"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/metricsauth"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/policy"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/sig"
)

// daemonAPIAddr is fixed by the spec: the sciond gRPC API is only exposed
// on localhost at the standard sciond port (spec: EnableDaemonAPI serves
// on 127.0.0.1:30255); it is deliberately not configurable.
const daemonAPIAddr = "127.0.0.1:30255"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// Initialize the scionproto logger; without this the embedded
	// gateway/daemon libraries log nothing, hiding control-plane errors
	// (e.g. remote gateway discovery failures).
	if err := scionlog.Setup(scionlog.Config{Console: scionlog.ConsoleConfig{Level: "info"}}); err != nil {
		log.Error("scion log setup failed", "err", err)
		os.Exit(1)
	}
	if err := run(log); err != nil {
		log.Error("agent failed", "err", err)
		os.Exit(1)
	}
}

// advertRefreshInterval bounds how quickly a Calico IPAM block allocated
// after startup (first pod on the node, node scale-up) becomes an
// advertised prefix with a working return path.
const advertRefreshInterval = 30 * time.Second

func run(log *slog.Logger) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	info, dynamicIPAM, err := nodeInfo(ctx, cfg)
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
	// Guardrail: pod-CIDR advertisement enabled but nothing to advertise.
	// With dynamic IPAM (Calico) a node legitimately has no block until
	// its first pod lands; the refresh loop below picks it up then.
	if err := checkAdvertisable(cfg, info, in.AdvertisedNets); err != nil {
		if !dynamicIPAM {
			return err
		}
		log.Warn("no pod CIDR to advertise yet; Calico allocates blocks on demand — will refresh", "err", err)
	}
	if err := renderPolicies(in, trafficPolicyFile, routingPolicyFile); err != nil {
		return fmt.Errorf("initial policy render: %w", err)
	}
	log.Info("policies rendered", "localIA", in.LocalIA, "advertised", in.AdvertisedNets)

	// reload is 1-buffered: the embedded gateway performs its own initial
	// blocking send on this channel to load policies at startup
	// (scion v0.15.1 gateway/gateway.go:367-371 sends ConfigReloadTrigger
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
		// Calico allocates IPAM blocks on demand: a node can gain its
		// first block (or additional ones) at any time, so the advertised
		// set must track the current allocation, not the startup snapshot.
		var tick <-chan time.Time
		if dynamicIPAM {
			t := time.NewTicker(advertRefreshInterval)
			defer t.Stop()
			tick = t.C
		}
		rerender := func(reason string) {
			in, err := policyInput(confDir, info, cfg)
			if err != nil {
				log.Error("policy input", "trigger", reason, "err", err)
				return
			}
			if err := renderPolicies(in, trafficPolicyFile, routingPolicyFile); err != nil {
				log.Error("policy re-render", "trigger", reason, "err", err)
				return
			}
			select {
			case reload <- struct{}{}:
			default:
			}
			log.Info("policies re-rendered, reload triggered",
				"trigger", reason, "advertised", in.AdvertisedNets)
		}
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-topoChanged:
				rerender("topology change")
			case <-tick:
				current, _, err := nodeInfo(ctx, cfg)
				if err != nil {
					log.Error("node info refresh", "err", err)
					continue
				}
				if slices.Equal(current.PodCIDRs, info.PodCIDRs) {
					continue
				}
				info = current
				rerender("pod CIDR change")
			}
		}
	})

	// sigUp is closed once the gateway's standalone daemon connector is
	// constructed. The connector (scionproto pkg/daemon/standalone.go)
	// MustRegisters metrics that daemonapi.Run also registers; daemonapi
	// registers tolerantly, so it must go second — gate it on the gateway.
	// TODO: this start-ordering gate is a workaround for scionproto's
	// MustRegister on the global default registry; if upstream ever takes
	// an injectable registry, the gate (and sigUp) can go away.
	sigUp := make(chan struct{})
	sigUpOnce := closeOnce(sigUp)
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
				sigUpOnce()
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
		if cfg.MetricsTLSCert != "" {
			// service-ca populates the mounted secret asynchronously
			// after pod start; wait for the files instead of racing.
			if err := waitForFiles(ctx, log, cfg.MetricsTLSCert, cfg.MetricsTLSKey); err != nil {
				return err
			}
			rc, err := rest.InClusterConfig()
			if err != nil {
				return fmt.Errorf("in-cluster config for metrics auth: %w", err)
			}
			cs, err := kubernetes.NewForConfig(rc)
			if err != nil {
				return err
			}
			// Probes bypass auth (kubelet sends no token and must not
			// depend on the apiserver); everything else on this mux —
			// /metrics and the gateway debug pages — requires a token
			// allowed to `get` /metrics.
			srv.Handler = metricsauth.Middleware(mux, cs, "/healthz", "/readyz")
			log.Info("serving metrics/health", "addr", cfg.MetricsAddr, "scheme", "https", "auth", "kubernetes")
			if err := srv.ListenAndServeTLS(cfg.MetricsTLSCert, cfg.MetricsTLSKey); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}
		log.Info("serving metrics/health", "addr", cfg.MetricsAddr, "scheme", "http", "auth", "none")
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
// nodeInfo determines this node's pod CIDRs and IP. The second return
// reports whether a dynamic-IPAM source (Calico BlockAffinity) is present:
// on such clusters the prefix set changes at runtime and callers must
// refresh it rather than trust the startup snapshot.
func nodeInfo(ctx context.Context, cfg config.Config) (kube.NodeInfo, bool, error) {
	if prefixes := os.Getenv("SCION_LOCAL_PREFIXES"); prefixes != "" {
		ip := os.Getenv("SCION_NODE_IP")
		if net.ParseIP(ip) == nil {
			return kube.NodeInfo{}, false, fmt.Errorf("SCION_NODE_IP required and must be a valid IP when SCION_LOCAL_PREFIXES is set, got %q", ip)
		}
		return kube.NodeInfo{PodCIDRs: split(prefixes), InternalIP: ip}, false, nil
	}
	rc, err := rest.InClusterConfig()
	if err != nil {
		return kube.NodeInfo{}, false, fmt.Errorf("in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return kube.NodeInfo{}, false, err
	}
	info, err := kube.GetNodeInfo(ctx, cs, cfg.NodeName)
	if err != nil {
		return info, false, err
	}
	// Calico IPAM: pod prefixes live in BlockAffinity objects. They take
	// priority over node.spec.podCIDR(s): on clusters with the node CIDR
	// allocator enabled the Node carries a pod CIDR that Calico ignores,
	// so advertising it would misroute return traffic. Absence of the
	// BlockAffinity API means this is not a Calico cluster; fall through
	// to the Node sources.
	dc, err := dynamic.NewForConfig(rc)
	if err != nil {
		return info, false, err
	}
	calicoCIDRs, err := kube.CalicoPodCIDRs(ctx, dc, cfg.NodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return info, false, nil
		}
		return info, false, err
	}
	if len(calicoCIDRs) > 0 {
		info.PodCIDRs = calicoCIDRs
	}
	return info, true, nil
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
// checkAdvertisable fails when pod-CIDR advertisement is enabled but no
// IPv4 prefix was discovered. CNIs with their own IPAM (Calico, Cilium
// cluster-pool) leave node.spec.podCIDR(s) empty, and IPv6-only allocations
// are filtered because the dataplane is IPv4-only. Silently advertising
// nothing would bring the tunnel up with no return path to this node's pods
// — fail loudly instead.
func checkAdvertisable(cfg config.Config, info kube.NodeInfo, nets []string) error {
	if cfg.AdvertisePodCIDR && len(nets) == 0 {
		return fmt.Errorf("pod CIDR advertisement is enabled but no IPv4 pod CIDR was discovered "+
			"(node %s: podCIDRs=%v); the CNI must use node-CIDR-allocator IPAM, "+
			"or set SCION_LOCAL_PREFIXES, or disable spec.advertisement.podCIDR",
			cfg.NodeName, info.PodCIDRs)
	}
	return nil
}

func advertisedNets(info kube.NodeInfo, cfg config.Config) []string {
	var nets []string
	if cfg.AdvertisePodCIDR {
		for _, cidr := range info.PodCIDRs {
			// The dataplane and policy engine are IPv4-only; advertising
			// a dual-stack node's IPv6 pod CIDR makes the remote SIG
			// install an IPv6 route toward a gateway that cannot serve
			// it (blackhole). Mirror the operator's IPv4-only filtering.
			if prefix, err := netip.ParsePrefix(cidr); err == nil && prefix.Addr().Is4() {
				nets = append(nets, cidr)
			}
		}
	}
	if cfg.AdvertiseNodeIP {
		// Same IPv4-only rationale as above for IPv6 node IPs.
		if ip := net.ParseIP(info.InternalIP); ip != nil && ip.To4() != nil {
			nets = append(nets, info.InternalIP+"/32")
		}
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

// closeOnce returns a function that closes ch exactly once; further calls
// are no-ops. The gateway's OnUp is documented to fire once, but readiness
// callbacks are exactly the kind of contract that shifts across scionproto
// bumps, and a double close of sigUp would panic the whole agent.
func closeOnce(ch chan<- struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// waitForFiles blocks until every path exists (or ctx is canceled). Used for
// the metrics TLS keypair: the OpenShift service-ca controller fills the
// mounted secret shortly after pod creation, so the very first pod start may
// briefly observe empty mounts.
func waitForFiles(ctx context.Context, log *slog.Logger, paths ...string) error {
	for {
		missing := ""
		for _, p := range paths {
			if _, err := os.Stat(p); err != nil {
				missing = p
				break
			}
		}
		if missing == "" {
			return nil
		}
		log.Info("waiting for TLS material", "path", missing)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
