# SCION Kubernetes Operator — Design

Date: 2026-07-23
Updated: 2026-08-20
Status: Implemented and live-validated
Repo: github.com/mkowalski/scion-k8s-operator

## 1. Goal

Make every node of an OpenShift cluster a first-class SCION endhost with
transparent, bidirectional IP-over-SCION connectivity, delivered as a
Kubernetes operator. OpenShift 5.x is the primary target; a clean path to
vanilla Kubernetes is preserved but not implemented in v1.

The architecture below is the as-built v0.1 design. OpenShift 5.0/RHCOS 10.2,
OVN-Kubernetes local-gateway routing, source preservation, SCION v0.15.1,
registrar lifecycle, and the two-AS development topology were validated live
on a five-node cluster on 2026-08-20.

"First-class endhost" means each node:

1. Runs a SCION daemon (path lookup, trust engine) exposing the standard
   gRPC API on `127.0.0.1:30255`, so SCION-native workloads (snet, PAN,
   Anapaya scion-sdk) work out of the box.
2. Runs an embedded per-node SCION-IP gateway (micro-SIG): a `scion0` tun
   device in the host network namespace tunneling IP-in-SCION to remote
   SIGs, with dynamic prefix exchange (SGRP).
3. Advertises its own pod CIDR and, when explicitly enabled and safe, its node
   IP. These original source prefixes provide return reachability without a
   translated egress identity.

Non-SCION-aware workloads need zero changes. On OVN-Kubernetes,
`routingViaHost: true` sends pod egress through `ovn-k8s-mp0` into the host
routing table. Only routes learned from healthy SGRP sessions select `scion0`;
the agent performs no source NAT. Preserving pod sources also requires OVN
route advertisements for the default `PodNetwork`. Inbound traffic uses the
existing host-to-pod path.

## 2. Background and key research findings

- Modern SCION v0.15.1 endhosts are userspace components: topology/TRCs,
  path/trust services, and UDP sockets; no kernel module or node reboot.
- The open-source SCION-IP Gateway supplies tun creation, IP-in-SCION framing,
  SGRP prefix exchange, and Linux route publication.
- Endhost discovery supports URL, DNS-SRV, DHCP option 72, or mDNS and fetches
  `topology.json` plus TRCs.
- The per-node hostNetwork DaemonSet pattern is proven on OpenShift. Live RHCOS
  testing requires privileged/root because SELinux blocks the narrower
  capability-only host tun/state access.
- Open-source ASes, Anapaya EDGE, and SCIONLab can supply the AS-side control
  service and border router; the border router never moves into the node.

## 3. Architecture

Three binaries and images share one repository:

```text
scion-operator (Deployment, namespace scion-system)
  watches: ScionNetwork, DaemonSet, Nodes, OpenShift Network,
           OVN RouteAdvertisements
  manages: scion-node-agent DaemonSet, agent RBAC/SCC, aggregate status
  reads:   OVN source-preservation prerequisites (never mutates them)
        |
        v
scion-node-agent (DaemonSet, every node, hostNetwork)
  +----------------+  +----------------+  +--------------------------+
  | bootstrap      |  | daemon module  |  | gateway module (SIG)     |
  | topology + TRC |->| paths, trust,  |->| tun scion0 in host netns |
  | discovery      |  | gRPC :30255    |  | SGRP advertise/learn     |
  +----------------+  +----------------+  +--------------------------+
```

`scion-registrar` is the third binary. It runs beside an open-source AS
control service, not in the cluster, and atomically reconciles the
operator-managed `sigs` set.

The agent is a single Go binary embedding `scionproto/scion` packages
(`daemon/`, `gateway/`) as libraries plus bootstrap logic modeled on
`netsec-ethz/bootstrapper`. Gateway-to-daemon communication is in-process;
the localhost gRPC API is additionally exposed for node-local SCION-aware
applications.

### 3.1 Operator

Scaffolded with operator-sdk/kubebuilder (controller-runtime). Duties:

- Reconcile the `ScionNetwork` singleton into the agent DaemonSet,
  ServiceAccount, RBAC, and OpenShift SCC. Repair drift.
- Validate spec via CRD structural schema + CEL rules.
- Aggregate agent readiness, registrar state, and platform prerequisites into
  `Available`, `Progressing`, and `Degraded`; publish the per-node summary and
  discovered ISD-AS.
- Observe `Network.operator.openshift.io/cluster` and accepted default-network
  `PodNetwork` `RouteAdvertisements`; report failures without mutating either.
- Handle upgrades: new operator version rolls the DaemonSet
  (RollingUpdate, maxUnavailable 10%).
- Run the registrar controller: reconcile the AS-side SIG registration
  for the current node set via the configured backend (see 3.6).
- OpenShift/vanilla divergence is isolated: SCC management is gated on
  API-group availability; on vanilla k8s the operator skips SCC and
  relies on namespace Pod Security Admission `privileged`.

### 3.2 ScionNetwork CRD

Cluster-scoped, singleton named `cluster` (same convention as OpenShift
`Network`/`Proxy` config objects). API group: `scion.mkowalski.github.io`,
version `v1alpha1` (group is user-controlled via GitHub Pages domain;
revisit before any public release).

`spec`:
- `bootstrap`: discovery mode (`url` | `dns` | `dhcp` | `mdns`), mode-specific
  fields, optional pinned-TRC Secret, refresh interval.
- `advertisement`: pod CIDR and node IP switches (both default true; node IP
  must be disabled if it overlaps the underlay).
- `acceptPolicy`: allowed remote ISD-ASes, user-forbidden IPv4 ranges, and
  required node-to-AS `underlayCIDRs`.
- `dataplane`: tun device name (default `scion0`).
- `registrar`: backend (`manual` | `http` | `anapaya`), endpoint and Secret.
- `nodeSelector`, tolerations, and agent image override.

`status`:
- `conditions`: Available, Progressing, Degraded.
- `isdAS`: local ISD-AS for URL bootstrap mode.
- `nodes`: ready, total, and unready-node names.
- `registrar`: registered count, desired SIGs, last sync and last error.

### 3.3 Node agent

- **Bootstrap module**: discovers the local AS by URL, DNS-SRV, DHCP option 72,
  or mDNS; fetches topology/TRCs; validates optional pinned TRCs; updates the
  host cache atomically on a refresh schedule.
- **Daemon module**: `NewStandaloneConnector` supplies in-process path and
  trust services; the standard API remains available on `127.0.0.1:30255`.
- **Gateway module**: creates `scion0`, advertises the pod CIDR and optional
  node IP, learns exact remote prefixes through SGRP, and lets the upstream
  route publisher install them.
- **Route guardrails**: subtract cluster, service, user-forbidden, and
  `underlayCIDRs` ranges from accepted IPv4 prefixes. The agent installs no
  SNAT, policy routing, eBPF, or OVN state.

### 3.4 Traffic flow

- Egress (pods): pod → OVN-K local gateway (`ovn-k8s-mp0`) → host routing
  table → an exact learned route selects `scion0` → IP-in-SCION frames over UDP
  to the remote SIG. The original pod source remains unchanged.
- Egress (node/host processes): host routing table → an exact learned route →
  `scion0`, retaining the host-selected source address.
- Other destinations: no matching SCION route exists, so the normal host uplink
  remains selected.
- Ingress: remote SIG → this node's SIG → decapsulate on `scion0` → host
  routing → local pod CIDR via `ovn-k8s-mp0`, or node itself.

### 3.5 Security

- Dedicated `scion-system` namespace with privileged PSA labels.
- The live RHCOS result requires the agent to run privileged as root; the
  operator manages the corresponding SCC. A narrower SELinux policy remains
  future hardening.
- Agent RBAC is read-only for its own Node data; operator Secret access is
  namespaced to `scion-system`.
- Bootstrap pins and registrar credentials are mounted read-only. Plain SCION
  endhosts otherwise hold no private AS keys.
- DaemonSet tolerates all node roles and uses `system-node-critical` priority.

### 3.6 AS-side auto-registration (registrar)

Remote SIGs discover this cluster's node SIGs via the AS control
service's `DiscoveryService.Gateways` RPC, fed from `sigs` entries in the
AS `topology.json` (or the Anapaya EDGE equivalent). Egress works
without registration; inbound requires it. To support node churn and
autoscaling, the operator includes a **registrar controller**: it
watches Nodes and reconciles the AS-side SIG list through a pluggable
backend interface (backend selected in `spec.registrar`):

- `manual` (default): no automation; the operator publishes the desired
  SIG list in `status` and a runbook documents the AS-side update.
- `http`: posts node join/leave to a small **registrar service** shipped
  by this project and run alongside the open-source control service (AS
  infrastructure, outside the cluster). The registrar authenticates
  requests (token from a Secret), patches `sigs` in `topology.json`, and
  reloads the control service. Used by dev/e2e environments and OSS ASes.
- `anapaya`: declared integration boundary; currently returns
  `ErrNotImplemented`. A real Appliance Management API backend requires
  appliance access and is outside v0.1.

HTTP deregistration runs under a finalizer. Failures retry until the ten-minute
deadline, after which the finalizer releases with a loud error to avoid wedging
cluster deletion. The registrar reconciles the full set; it does not implement
heartbeat expiry.

In parallel, an upstream design proposal will be filed with
`scionproto/scion` for TTL/heartbeat-based dynamic SIG self-registration
in the control service, which would eventually collapse the backends
into one standard mechanism.

## 4. Ecosystem fit

- Day-2 add-on. No `install-config.yaml` changes, no MachineConfig, no
  RHCOS layering, no reboots, no CNI modifications. "SCION from day 0"
  is achieved via GitOps (ArgoCD/ACM) applying the operator right after
  install.
- Distribution: OLM bundle (CSV) for OpenShift, plus plain
  kustomize manifests kept vanilla-Kubernetes-compatible.
- Monitoring: static ServiceMonitor and PrometheusRule manifests integrate
  with OpenShift user-workload monitoring. Embedded SCION metrics plus agent
  and operator health cover the implemented observability surface.
- Upgrades independent of OCP upgrades.

## 5. Failure modes

- Agent pod down: `scion0` and learned routes vanish with the process; traffic
  fails closed until the DaemonSet recreates the agent.
- AS infrastructure unreachable: path/SIG discovery and registrar sync fail;
  status and alerts surface the fault while existing agent processes remain.
- Platform prerequisites missing: `Available=False` and `Degraded=True` with
  `HostRoutingDisabled` or `SourcePreservationDisabled`; the operator does not
  repair cluster-wide OVN configuration.
- Underlay omitted from `acceptPolicy.underlayCIDRs`: learned routes can capture
  discovery, registrar, probe, or border-router transport and deadlock the
  attachment. Installation documentation treats the field as required.
- Operator down: agents continue forwarding; reconciliation and status pause.
- Registrar unavailable: current data-plane sessions continue, but new inbound
  reachability and finalizer cleanup wait for sync or the deletion deadline.

## 6. Testing

- Unit and envtest: bootstrap, policy subtraction, upstream parsers, rendered
  resources, registrar behavior, status precedence, and platform detection.
- Integration: discovery protocol and agent/gateway behavior in namespaces.
- Live e2e: five-node OpenShift 5.0/RHCOS 10.2 with a two-AS SCION v0.15.1
  topology. The hardened suite verifies exact `scion0` selection, ordinary and
  underlay route exclusions, pod and host source preservation on remote `sigb`,
  TCP, bidirectional ICMP, registration, pod replacement, and full cleanup.
  SCIONLab user-AS available for realistic manual testing.

## 7. Non-goals (v1)

- Vanilla Kubernetes support (path preserved; SCC logic isolated).
- Cluster control-plane/boot-time traffic over SCION.
- NAT traversal between node and border router.
- Native IPv6-over-SCION (dual-stack clusters run the IPv4 path; IPv6 cluster
  and service CIDRs are excluded from the IPv4-only policy input).
- Anapaya registrar backend implementation (interface and stub tests in
  v1; real-appliance validation in v1.x).
- Multipath/path-policy tuning beyond defaults.

## 8. Risks

- scionproto `private/` and gateway internals can change; versions are pinned
  and bumps require local plus live validation.
- Privileged root remains broader than desired; a tailored SELinux policy is
  the path to capability-scoped operation.
- Per-node SIG fan-out is N nodes × M remote gateways and needs scale testing.
- The HTTP registrar is plaintext unless deployed behind a trusted tunnel or
  TLS proxy; the Anapaya backend remains a stub.
- Node-IP advertisement can recursively capture a single-NIC underlay. It must
  remain disabled until overlap detection is implemented.

## 9. As-built notes and remaining deviations

- Bootstrapper protocol corrections: TRC blobs are fetched from
  `/trcs/isd{I}-b{B}-s{S}/blob` (not `/trcs/<file>`), and TRCs are stored
  with upstream's uppercase naming `ISD{I}-B{B}-S{S}.trc`.
- TRC pinning compares fetched TRCs against pin-file bytes mounted from
  the bootstrap Secret (`<stateDir>/pinned-trcs/<name>`), not
  trust-on-first-use.
- The gateway embeds the daemon via `daemon.NewStandaloneConnector` (no
  gRPC hop); the standard sciond gRPC API on `127.0.0.1:30255` is a
  separate opt-in module (`SCION_ENABLE_DAEMON_API`, default on).
- The embedded gateway's SIG control-plane ID is `scion-node-agent`
  (upstream default is `gateway`).
- Prefix guardrails are implemented as SGRP accept-policy prefix
  subtraction (forbidden CIDRs carved out of accepted prefixes); no
  custom netlink filtering layer.
- Control-service topology reload on `sigs` changes was verified on v0.15.1;
  SIGHUP re-reads the file and the registrar defaults to
  `systemctl kill -s HUP scion-control`.
- The registrar's heartbeat-based entry expiry (§3) was replaced by
  full-set reconciliation on every PUT; unclean cluster removal can leave
  stale entries until the next sync or manual cleanup
  (docs/as-registration.md).
- The `scion-system` Namespace is applied but deliberately NOT owner-ref'd
  to the ScionNetwork: garbage-collecting it on CR deletion would delete
  the operator itself. Namespace lifecycle belongs to the deploy manifests.
- The ScionNetwork singleton is enforced via a CEL rule
  (`metadata.name == 'cluster'`), not an admission webhook.
- A UWM-safe alert (`ScionNodeAgentAbsent`, based on `absent(up{...})`)
  was added because `kube_daemonset_status_number_unavailable` is a
  platform metric invisible to the user-workload-monitoring ruler.
- Authenticated bootstrap is not implemented: `spec.bootstrap.secretRef`
  only mounts pinned TRCs (keys are TRC filenames).
- `status.prefixes` (advertised/learned counts) is not implemented; it
  needs an agent→operator reporting channel that does not exist yet
  (future work).
- Dataplane MTU overrides are not implemented; the MTU comes from the AS
  topology.
- The agent's dedicated bootstrap-state metric is not implemented; only
  the embedded SCION components' own metrics are exposed.
- ServiceMonitor and PrometheusRule ship as static manifests in
  config/manifests, not as operator-managed objects.
- `status.isdAS` is populated by the operator only for the `url`
  bootstrap mode (the operator itself fetches `<discoveryURL>/topology`);
  for dns/dhcp/mdns modes discovery happens on the nodes and the field
  stays empty (future: agent-reported).
- OVN-K shared-gateway mode was proven to bypass the host routing table.
  Transparent pod egress now requires `routingViaHost: true`; preserving pod
  sources additionally requires an accepted default-network `PodNetwork`
  RouteAdvertisements configuration. The operator observes these prerequisites
  through existing conditions and never modifies OVN state.
