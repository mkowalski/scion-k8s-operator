# SCION Kubernetes Operator — Design

Date: 2026-07-23
Status: Draft for review
Repo: github.com/mkowalski/scion-k8s-operator

## 1. Goal

Make every node of an OpenShift cluster a first-class SCION endhost with
transparent, bidirectional IP-over-SCION connectivity, delivered as a
Kubernetes operator. OpenShift 5.x is the primary target; a clean path to
vanilla Kubernetes is preserved but not implemented in v1.

Note: the platform research backing this design (SCC behavior, OVN-K
shared-gateway SNAT, user-workload monitoring, MCO alternatives) was
validated against OpenShift 4.x documentation and code. These mechanisms
are expected to carry over to 5.x, but each assumption must be
re-verified against 5.x during implementation; divergences (e.g.
bootc-based RHCOS changes) are handled in the implementation plan.

"First-class endhost" means each node:

1. Runs a SCION daemon (path lookup, trust engine) exposing the standard
   gRPC API on `127.0.0.1:30255`, so SCION-native workloads (snet, PAN,
   Anapaya scion-sdk) work out of the box.
2. Runs an embedded per-node SCION-IP gateway (micro-SIG): a `scion0` tun
   device in the host network namespace tunneling IP-in-SCION to remote
   SIGs, with dynamic prefix exchange (SGRP).
3. Advertises its own reachability into the SCION network: node primary
   IP (/32) and the node's pod CIDR, enabling inbound traffic to node and
   pods.

Non-SCION-aware workloads need zero changes: pod egress to remote SCION
prefixes is steered via host routes (OVN-Kubernetes SNATs pod egress to
the node IP through the host routing table); inbound traffic to pod IPs
is delivered via the host-to-pod path OVN-K already provides
(`ovn-k8s-mp0`).

## 2. Background and key research findings

- Modern SCION (v0.15+, post dispatcher removal) endhost stack is minimal:
  `scion-daemon` + `topology.json` + TRCs. Pure Go, userspace, UDP-only,
  no kernel modules, no per-host secrets. Apps bind plain UDP sockets in
  the AS `dispatched_ports` range.
- The SCION-IP Gateway is fully open source in `scionproto/scion`
  (`gateway/`): tun device, IP-in-SCION framing, SGRP prefix exchange,
  route programming. Anapaya GATE is a commercial hardened variant.
- Endhost bootstrapping is solved upstream (`netsec-ethz/bootstrapper`):
  discovery via explicit URL, DNS-SD/SRV, DHCP option 72, or mDNS; fetch
  `topology.json` + TRCs over HTTP(S).
- No production SCION/Kubernetes integration exists anywhere; this is
  greenfield. Closest prior art for the per-node pattern: Submariner
  route-agent, kilo, Tailscale on k8s.
- SCION requires nothing kernel-level, so no MachineConfig, no RHCOS
  layering, no reboots: a privileged/NET_ADMIN hostNetwork DaemonSet
  suffices.
- AS attachment works with both commercial Anapaya EDGE appliances and
  self-run open-source ASes (control service + border router), including
  the free SCIONLab testbed for development.

## 3. Architecture

Two components, one repository, two container images:

```
scion-operator (Deployment, 1 replica, namespace scion-system)
  watches:  ScionNetwork (cluster-scoped CRD, singleton "cluster")
  manages:  scion-node-agent DaemonSet, SCC, ServiceAccount, RBAC,
            ServiceMonitor, PrometheusRule, aggregated status
        |
        v
scion-node-agent (DaemonSet, every node, hostNetwork)
  +----------------+  +----------------+  +--------------------------+
  | bootstrap      |  | daemon module  |  | gateway module (SIG)     |
  | topology + TRC |->| paths, trust,  |->| tun scion0 in host netns |
  | discovery      |  | gRPC :30255    |  | SGRP advertise/learn     |
  +----------------+  +----------------+  | route programming        |
                                          +--------------------------+
```

The agent is a single Go binary embedding `scionproto/scion` packages
(`daemon/`, `gateway/`) as libraries plus bootstrap logic modeled on
`netsec-ethz/bootstrapper`. Gateway-to-daemon communication is in-process;
the localhost gRPC API is additionally exposed for node-local SCION-aware
applications.

### 3.1 Operator

Scaffolded with operator-sdk/kubebuilder (controller-runtime). Duties:

- Reconcile the `ScionNetwork` singleton into: DaemonSet, ServiceAccount,
  RBAC, SCC (OpenShift), monitoring objects. Repair drift.
- Validate spec via CRD structural schema + CEL rules (no admission
  webhook in v1).
- Aggregate per-node agent health into `status.conditions`
  (`Available`, `Progressing`, `Degraded` — ClusterOperator style) and a
  per-node summary (ready/total, degraded nodes with reasons). Source of
  per-node state: agent pod readiness plus agent-published state (exact
  mechanism — Lease vs. status endpoint — decided in the implementation
  plan; node annotations are avoided).
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
- `bootstrap`: mode (`url` | `dns` | `dhcp` | `mdns`), discovery URL,
  optional secret ref for authenticated bootstrap (Anapaya deployments),
  optional pinned TRCs, refresh interval.
- `advertisement`: advertise pod CIDR (bool, default true), advertise
  node IP (bool, default true).
- `acceptPolicy`: allowed remote ISD-ASes and IP prefix filters for
  learned routes.
- `dataplane`: tun device name (default `scion0`), MTU overrides.
- `registrar`: backend (`manual` | `http` | `anapaya`), endpoint URL,
  credential secret ref (see 3.6).
- `nodeSelector` / tolerations passthrough for the DaemonSet.

`status`:
- `conditions`: Available, Progressing, Degraded.
- `isdAs`: the ISD-AS the cluster is attached to.
- `nodes`: readyCount, totalCount, degraded list with reasons.
- `prefixes`: advertised count, learned count.
- `registrar`: registered node count, desired SIG list (for `manual`
  mode), last sync time/error.

### 3.3 Node agent

- **Bootstrap module**: resolves AS attachment at startup and on a
  refresh schedule. Order: explicit URL → DNS-SD/SRV → DHCP option 72 →
  mDNS. Verifies TRC chains; caches under hostPath
  `/var/lib/scion-node-agent`. Works identically against Anapaya EDGE
  discovery endpoints and OSS ASes.
- **Daemon module**: embedded sciond; sqlite path/trust caches on the
  hostPath; gRPC on `127.0.0.1:30255`.
- **Gateway module**: creates `scion0`; advertises node pod CIDR (from
  its own Node object `spec.podCIDRs`; node name via downward API; RBAC:
  `get` nodes only) and node IP /32 via SGRP; learns remote prefixes via
  control-service gateway discovery + SGRP filtered by `acceptPolicy`;
  programs host routes for learned prefixes, tagged with a dedicated
  route protocol ID for safe identification and cleanup.
- **Route guardrails**: refuses to install prefixes overlapping
  clusterNetwork, serviceNetwork, machineNetwork, or the default route.
  Never touches `br-ex` or OVN-K-owned state.

### 3.4 Traffic flow

- Egress (pods): pod → OVN-K shared-gateway SNAT to node IP → host
  routing table → `scion0` → IP-in-SCION frames over UDP to remote SIG.
- Egress (node/host processes): host routing table → `scion0`.
- Ingress: remote SIG → this node's SIG (reachable at advertised node
  IP) → decapsulate on `scion0` → host routing → local pod CIDR via
  `ovn-k8s-mp0`, or node itself.
- Symmetry: each node advertises only its own pod CIDR, so return
  traffic lands on the correct node; no cross-node NAT.

### 3.5 Security

- Dedicated namespace `scion-system`.
- Custom SCC: `hostNetwork`, capability `NET_ADMIN`, hostPath volumes
  (`/var/lib/scion-node-agent`, `/dev/net/tun`), non-root with
  capabilities preferred. Fallback to `privileged: true` only if SELinux
  `container_t` blocks host tun access — validated during
  implementation; both modes documented.
- Agent RBAC: `get` on Nodes; nothing else cluster-scoped.
- Secrets only when the AS requires authenticated bootstrap; plain SCION
  endhosts hold no private keys. TRCs are public trust material.
- DaemonSet: tolerations for all node roles, priorityClassName
  `system-node-critical`.

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
- `anapaya`: CRUDs gateway entries via the Anapaya appliance management
  REST API (OpenAPI; a generated client exists in
  `Anapaya/ansible-collections`), credential from a Secret. Developed
  against a stub derived from the OpenAPI models until real appliance
  access is available; planned for v1.x, interface defined in v1.

Deregistration: registrar removes entries on node deletion; the
registrar service additionally expires entries whose agent stops
heartbeating (protects against unclean cluster removal).

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
- Monitoring: OpenShift user-workload monitoring; ServiceMonitor +
  PrometheusRule shipped by the operator. Agent and embedded SCION
  components export Prometheus metrics natively; agent adds bootstrap
  state, session health, advertised/learned prefix and route counts.
- Upgrades independent of OCP upgrades.

## 5. Failure modes

- Agent pod down: tun and routes vanish with the process; traffic to
  remote prefixes fails closed (no fallback leak to plain IP). Routes
  are cleaned on shutdown and stale ones reconciled on start using the
  dedicated route protocol ID.
- AS infrastructure unreachable: paths expire, SIG sessions drop; agent
  retries, readiness goes false, Degraded condition and alert fire.
- OVN-K upgrades/migrations: agent owns only `scion0` and its tagged
  routes; no interaction with OVN-K state.
- Upstream churn: `scionproto/scion` is pinned; internal `private/`
  packages may move between releases; version bumps are gated by e2e.
- Operator down: agents keep running (data plane unaffected); only
  reconciliation/status pauses.
- Registrar backend unreachable: existing registrations persist
  (inbound keeps working for current nodes); new nodes get egress-only
  connectivity until sync succeeds; Degraded condition and alert fire.

## 6. Testing

- Unit: route programming (mocked netlink), prefix guardrails, bootstrap
  parsing/verification, SGRP advertisement composition, CRD validation,
  registrar backends (HTTP registrar and Anapaya API stubbed).
- Integration: docker-compose SCION topology (scionproto `tools/`) with
  the agent in network namespaces; bidirectional ping/TCP between two
  simulated nodes across the SCION topology; registrar service
  patching `topology.json` and reloading the control service.
- E2E: OpenShift cluster(s) in CI attached to a dev AS; verify pod↔pod
  cross-cluster over SCION, inbound reachability, node reboot, node
  add/remove with automatic (de)registration, agent and operator rolling
  updates, guardrail enforcement, status aggregation.
  SCIONLab user-AS available for realistic manual testing.

## 7. Non-goals (v1)

- Vanilla Kubernetes support (path preserved; SCC logic isolated).
- Cluster control-plane/boot-time traffic over SCION.
- NAT traversal between node and border router.
- IPv6-only clusters (designed for, not tested).
- Anapaya registrar backend implementation (interface and stub tests in
  v1; real-appliance validation in v1.x).
- Multipath/path-policy tuning beyond defaults.

## 8. Risks

- SCION library surface: `gateway/` and `daemon/` internals are not a
  stable public API; embedding may require forking or upstreaming
  refactors. Mitigation: pin versions, minimize touched surface,
  engage upstream early.
- SELinux vs. non-privileged tun access on RHCOS: may force
  `privileged: true` initially.
- Per-node SIG scalability: N nodes × M remote SIGs sessions; SGRP fan-out
  needs measurement at moderate cluster sizes.
- Registrar: the HTTP registrar service is new AS-side infrastructure to
  operate and secure; the Anapaya backend depends on an appliance API we
  can only stub until real access exists (API observed at version 0.1.0,
  may change); control-service topology reload behavior on `sigs`
  changes must be validated.
- AS-side registration friction with autoscaling node pools.

## 9. Implementation deviations (as built)

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
- Control-service topology reload on `sigs` changes was verified: SIGHUP
  re-reads the file (v0.15.0 `private/topology/reload.go`); the registrar
  defaults to `systemctl kill -s HUP scion-control`.
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
