# Research Notes: SCION Ecosystem and OpenShift Integration

Research performed 2026-07 (pre-implementation), backing the design in
`superpowers/specs/2026-07-23-scion-k8s-operator-design.md`. Sources: local
clones of scionproto/scion (v0.15.0) and netsec-ethz/bootstrapper, GitHub org
surveys, OpenShift 4.x documentation and openshift/* repositories.

## Where the border router fits

The BR is the piece deliberately NOT moved into the node. Roles in an AS:

| Component | Plane | In our design |
|---|---|---|
| Control service | Control: beaconing, path segments, TRCs, gateway discovery | AS infrastructure |
| Border router (BR) | Data: forwards SCION packets between ASes; delivers packets to endhosts inside the AS as plain UDP/IP | AS infrastructure |
| Endhost stack (daemon + SIG) | Data edge: path selection, IP-in-SCION encap/decap | moved into every node |

Every packet's path: pod → OVN-K SNAT → host route → `scion0` (node SIG
encapsulates) → **UDP to the local AS border router** → inter-AS hops →
destination AS's BR → UDP to the remote SIG → decap. Inbound is the mirror:
our BR receives SCION packets from the network and delivers them as plain
UDP to the node SIG's data port (30056). A node never speaks SCION wire
format to anything except its local BR.

Consequences:
- `topology.json` (fetched at bootstrap) matters chiefly because it carries
  the **BR internal interface addresses** — where the node sends every SCION
  packet — plus control service addresses and `sigs`/`dispatched_ports`.
- The UDP reachability requirement (30056/30256/30856) is node ↔ AS
  infrastructure; the BR is the only data-plane dependency of a node.
- The BR is a shared hairpin: per-node SIGs parallelize encapsulation, but
  all cluster ↔ SCION traffic funnels through the AS's BR(s). ASes scale by
  running multiple BRs; the daemon picks per selected path.
- Moving the BR into nodes (cilion-style, node-as-router) would make the
  cluster an AS attachment point: inter-AS links, interface IDs allocated by
  neighbors, beaconing participation, and transit failure domains — rejected
  to keep nodes plain endhosts (no keys, no per-link peering; see
  decisions.md D14).

## SCION endhost stack (scionproto/scion, v0.15.0+)

- Post dispatcher-removal, a first-class endhost is minimal: **`scion-daemon`
  + `topology.json` + TRCs**. Pure Go, userspace, UDP-only underlay, no
  kernel modules, no per-host secrets. Applications bind plain UDP sockets in
  the AS `dispatched_ports` range (recommended 31000-32767); the legacy shim
  dispatcher (port 30041) is only needed for legacy port ranges.
- Daemon gRPC API on `127.0.0.1:30255` serves path/AS/TRC queries (snet, PAN,
  Anapaya scion-sdk clients).
- **`pkg/daemon.NewStandaloneConnector`** (new in v0.15.0) runs path lookup +
  trust engine in-process — an embedder needs no sciond gRPC hop at all
  (`pkg/daemon/standalone.go:109`).
- **The SCION-IP Gateway (SIG) is fully open source** (`gateway/` in
  scionproto/scion, Apache-2.0): IP-in-SCION tunneling over a tun device,
  session/stream framing (`doc/sig.rst`), SGRP prefix exchange
  (`proto/gateway/v1/prefix.proto`), Linux route programming via netlink
  (`gateway/routemgr`). `gateway.Gateway.Run` handles tun creation and routes
  internally; an embedder only fills the struct
  (`gateway/gateway.go:143`, wiring reference `gateway/cmd/gateway/main.go`).
- SIG discovery: remote SIGs found via the control service
  `DiscoveryService.Gateways` RPC, fed from `sigs` entries in the AS
  `topology.json` (`private/topology/json/json.go`, `GatewayInfo{ctrl_addr,
  data_addr}`). This is why AS-side registration exists (see registrar).
- Ports: SIG ctrl 30256 (QUIC/SCION), data 30056/udp, probe 30856/udp;
  daemon API 30255/tcp localhost; legacy endhost 30041/udp.
- Control service reloads `topology.json` on SIGHUP
  (`control/cmd/control/main.go` → `private/topology/reload.go`).
- Builds: Bazel; releases ship deb/rpm tarballs (v0.15.0), no published
  container images; distroless CI images only. Go 1.26, CGO-free, statically
  linkable.

## Endhost bootstrapping (netsec-ethz/bootstrapper)

- Design "completed externally" per scionproto
  `doc/dev/design/endhost-bootstrap.rst`. Discovery server found via explicit
  URL, DNS SRV `_sciondiscovery._tcp`, DHCP option 72, or mDNS; artifacts
  fetched over HTTP.
- Verified protocol (`fetcher/scion_openapi.go`): `GET /topology`,
  `GET /trcs` (JSON list of `{"id":{"isd","base_number","serial_number"}}`),
  `GET /trcs/isd{I}-b{B}-s{S}/blob`; TRCs cached as `ISD{I}-B{B}-S{S}.trc`.
  Note: the trailing `/blob` and uppercase filenames were discovered during
  implementation — early design assumptions were wrong.
- A plain endhost needs no private keys; TRCs are public trust material.

## Anapaya ecosystem

- **EDGE/CORE/GATE are commercial appliances** — no open-source appliance
  code. Managed via a REST **Appliance Management API** (OpenAPI, observed
  v0.1.0); the best public artifact is the generated Python client inside
  `Anapaya/ansible-collections` (`appliance_api_client`). NetBox/Nautobot
  plugins (`Anapaya/netbox-scion`) confirm the appliance inventory model.
- Anapaya's open endhost path is **`Anapaya/scion-sdk`** (Rust, Apache-2.0):
  SCION stack embedded in the application, no host daemon — the opposite
  philosophy to a host agent.
- `Anapaya/os-scion` is their fork of scionproto/scion (upstream/consume).

## Kubernetes/SCION prior art (as of 2026-07)

- **No production integration exists anywhere.** Only research prototype:
  `martenwallewein/cilion` (Cilium+SCION eBPF, early scaffolding).
- Closest host-agent prior art: `scionproto-contrib/scion-orchestrator`
  (cross-platform endhost/AS installer for the SCIERA network),
  `netsys-lab/scion-ip-translator`, Tailscale/WireGuard-over-SCION forks.
- **SCIONLab** (scionlab.org, netsec-ethz): free research testbed; register a
  "user AS", download config, attach over VPN/UDP — the practical dev/test
  environment for a real multi-AS SCION network.
- Debian packages: `packages.netsec.inf.ethz.ch` (SCIONLab),
  scionproto release tarballs, `scionproto/scion-packaging`.

## OpenShift per-node agent mechanisms

- **Privileged/NET_ADMIN hostNetwork DaemonSet is the proven pattern**
  (OVN-Kubernetes ovnkube-node, SR-IOV config daemon, node-exporter,
  Submariner route-agent, kilo, tailscaled-on-k8s). No reboots, day-2
  velocity, independent of OCP upgrades.
- MachineConfig+systemd only needed for boot-time/kubelet-independent
  networking (not our scope; every MC change reboots nodes). RHCOS image
  layering only for kernel-level needs (SCION has none). MC `extensions:` is
  a Red Hat-curated list — not usable for arbitrary RPMs.
- SELinux: privileged pods run `spc_t`; capability-only pods run
  `container_t`, which may block host `/dev/net/tun` — open risk, validate on
  real RHCOS (we ship NET_ADMIN-only and document privileged fallback).
- **OVN-Kubernetes interaction**: shared gateway mode SNATs pod egress to the
  node IP through the *host* routing table → host routes to `scion0` steer
  pod egress transparently, zero CNI changes. Inbound to pod IPs is
  host-reachable via `ovn-k8s-mp0`. OVN-K ignores foreign interfaces; never
  touch `br-ex`/default routes. On OVN-K, `node.spec.podCIDRs` may be empty —
  per-node subnet lives in annotation `k8s.ovn.org/node-subnets`.
- Monitoring: user-workload monitoring + ServiceMonitor/PrometheusRule.
  Caveat: platform kube-state-metrics metrics (e.g.
  `kube_daemonset_status_number_unavailable`) may not be visible to the UWM
  ruler — hence the additional `absent(up{...})`-based alert.
- OpenShift 5.x is the stated target; all of the above was validated against
  4.x docs/code and must be re-verified on 5.x (bootc-based RHCOS changes in
  particular).
