// Package sig embeds the scionproto SCION-IP gateway (SIG) in-process.
//
// Instead of connecting to a SCION daemon over gRPC (as the upstream
// gateway binary does), it uses daemon.NewStandaloneConnector so the
// agent needs no sidecar daemon. The gateway wiring mirrors upstream
// gateway/cmd/gateway/main.go (v0.15.0, realMain, lines 56-193).
package sig

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/scionproto/scion/gateway"
	"github.com/scionproto/scion/gateway/dataplane"
	"github.com/scionproto/scion/pkg/daemon"
	"github.com/scionproto/scion/pkg/private/serrors"
	"github.com/scionproto/scion/pkg/snet/addrutil"
	"github.com/scionproto/scion/private/env"
	"github.com/scionproto/scion/private/service"
)

// Default SIG ports, matching the upstream gateway config defaults
// (gateway/config/config.go: CtrlPort 30256, DataPort 30056, probe 30856).
const (
	ctrlPort  = 30256
	dataPort  = 30056
	probePort = 30856
)

// Params configures the embedded SCION-IP gateway.
type Params struct {
	ConfigDir         string        // contains topology.json + certs/
	TrafficPolicyFile string        // SIG traffic policy (session/path policies)
	RoutingPolicyFile string        // IP routing policy
	TunName           string        // tunnel device name, e.g. scion0
	NodeIP            net.IP        // node InternalIP; SIG binds ctrl/data here
	ReloadTrigger     chan struct{} // signaled to trigger policy reload; must not be nil
	// DebugMux, if non-nil, receives the gateway's diagnostic status pages
	// (engine, session configurator, IP routing policy, ...). Passing the
	// agent health server's mux exposes them there; if nil, a private mux
	// is created and the pages are not served anywhere (Task 11 wires this
	// into the health server).
	DebugMux *http.ServeMux
	// OnUp, if non-nil, is called once after the standalone daemon
	// connector is up and the gateway struct is constructed, immediately
	// before the blocking gateway run. It gates readiness on construction
	// (config/connector failures never fire it), but is still slightly
	// optimistic: tunnel device creation and control-plane startup happen
	// inside gateway.Gateway.Run, which offers no readiness callback.
	// Full data-plane readiness would need upstream scion support.
	OnUp func()
}

// addrs derives the control, data, and probe UDP addresses from the node
// IP. Mirrors upstream main.go lines 85-114: data and probe reuse the
// control IP with their own ports. IPv6 zones are deliberately dropped:
// Kubernetes node InternalIPs are zoneless.
func addrs(nodeIP net.IP) (ctrl, data, probe *net.UDPAddr) {
	ctrl = &net.UDPAddr{IP: nodeIP, Port: ctrlPort}
	data = &net.UDPAddr{IP: nodeIP, Port: dataPort}
	probe = &net.UDPAddr{IP: nodeIP, Port: probePort}
	return ctrl, data, probe
}

// Run starts the embedded SCION-IP gateway and blocks until ctx is done
// or the gateway fails. Tunnel device creation and route programming
// happen inside gateway.Gateway.Run.
func Run(ctx context.Context, p Params) error {
	if p.ReloadTrigger == nil {
		// A nil channel blocks forever: Gateway.Run's initial policy load
		// sends on ConfigReloadTrigger (gateway.go:369) and would silently
		// deadlock, leaving the gateway without any traffic policy.
		return serrors.New("ReloadTrigger must not be nil")
	}
	asinfo, err := daemon.LoadASInfoFromFile(filepath.Join(p.ConfigDir, "topology.json"))
	if err != nil {
		return serrors.Wrap("loading AS info from topology", err)
	}
	conn, err := daemon.NewStandaloneConnector(ctx, asinfo,
		daemon.WithCertsDir(filepath.Join(p.ConfigDir, "certs")),
		daemon.WithPeriodicCleanup(),
		daemon.WithMetrics(),
	)
	if err != nil {
		return serrors.Wrap("creating standalone daemon connector", err)
	}
	defer conn.Close()

	localIA, err := conn.LocalIA(ctx)
	if err != nil {
		return serrors.Wrap("retrieving local ISD-AS", err)
	}

	nodeIP := p.NodeIP
	if len(nodeIP) == 0 {
		// Fallback mirrors upstream main.go line 90.
		nodeIP, err = addrutil.DefaultLocalIP(ctx, daemon.TopoQuerier{Connector: conn})
		if err != nil {
			return serrors.Wrap("determining default local IP", err)
		}
	}
	ctrlAddr, dataAddr, probeAddr := addrs(nodeIP)
	pathMonitorIP, ok := netip.AddrFromSlice(ctrlAddr.IP)
	if !ok {
		return serrors.New("invalid IP address", "control", ctrlAddr.IP)
	}

	// AtomicRoutingTable implements both control.RoutingTableReader and
	// control.RoutingTableSwapper (upstream main.go line 152).
	routingTable := &dataplane.AtomicRoutingTable{}

	debugMux := p.DebugMux
	if debugMux == nil {
		debugMux = http.NewServeMux()
	}

	// Deviations from upstream main.go (lines 153-176):
	//   - ID is "scion-node-agent" instead of upstream's default "gateway"
	//     (config.Gateway.Validate defaults empty ID, config.go:114-116);
	//     it is only used as the StatusPages HTML title.
	//   - ConfigReloadTrigger is our policy-regeneration channel instead of
	//     app.SIGHUPChannel.
	//   - HTTPEndpoints starts without info/config/log-level pages (no TOML
	//     config to render); Run registers its own diagnostics pages into
	//     this map, so it must be non-nil (gateway.go lines 677-751).
	//   - HTTPServeMux is Params.DebugMux (or a private, unserved mux)
	//     instead of http.DefaultServeMux; see Params.DebugMux docs.
	//   - RouteSourceIPv4/IPv6 left nil (no source hint), DataAddr left nil;
	//     upstream leaves DataAddr unset as well.
	//   - RpcConfig is the zero env.RPC (all protocols enabled), matching
	//     upstream defaults when rpc config is absent.
	gw := &gateway.Gateway{
		ID:                       "scion-node-agent",
		TrafficPolicyFile:        p.TrafficPolicyFile,
		RoutingPolicyFile:        p.RoutingPolicyFile,
		ControlServerAddr:        ctrlAddr,
		ControlClientIP:          ctrlAddr.IP,
		ServiceDiscoveryClientIP: ctrlAddr.IP,
		PathMonitorIP:            pathMonitorIP,
		ProbeServerAddr:          probeAddr,
		ProbeClientIP:            ctrlAddr.IP,
		DataServerAddr:           dataAddr,
		DataClientIP:             dataAddr.IP,
		Daemon:                   conn,
		TunnelName:               p.TunName,
		RoutingTableReader:       routingTable,
		RoutingTableSwapper:      routingTable,
		ConfigReloadTrigger:      p.ReloadTrigger,
		HTTPEndpoints:            service.StatusPages{},
		HTTPServeMux:             debugMux,
		Metrics:                  gateway.NewMetrics(localIA),
		RpcConfig:                env.RPC{},
	}
	if p.OnUp != nil {
		p.OnUp()
	}
	// The tun device is created inside gw.Run; enable IPv4 forwarding on
	// it once it appears. OpenShift/RHCOS ships net.ipv4.ip_forward=0
	// with per-interface forwarding enabled only on CNI-managed
	// interfaces, so without this packets decapsulated onto the tun are
	// never forwarded to pod interfaces (verified live: inbound frames
	// reached scion0 but never ovn-k8s-mp0).
	go func() {
		path := filepath.Join("/proc/sys/net/ipv4/conf", p.TunName, "forwarding")
		for i := 0; i < 60; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			if err := os.WriteFile(path, []byte("1"), 0o644); err == nil {
				return
			}
		}
	}()
	return gw.Run(ctx)
}
