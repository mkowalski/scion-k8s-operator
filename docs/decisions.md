# Decision Log

Chronological record of the significant design and implementation decisions,
with the alternatives that were rejected and why. Companion to the design
spec (`superpowers/specs/2026-07-23-scion-k8s-operator-design.md`, esp. §9)
and `research.md`.

## D1: Transparent IP-over-SCION, bidirectional including inbound

"First-class SCION citizen" means workloads need zero changes: node and pod
egress to remote SCION prefixes rides SCION transparently, and remote SCION
sites can reach node/pod IPs. Rejected narrower scopes: SCION-native-apps-only
(daemon-only, no tun) and egress-only.

## D2: Every node runs the full endhost stack — no dedicated gateway nodes

Each node runs its own micro-SIG, advertising its pod CIDR and, only when
explicitly enabled and safe, its node IP. Rejected: Submariner-style dedicated
gateway nodes and virtual-SIG VIPs. SIG sessions are stateful point-to-point
with per-node prefixes; a VIP breaks session affinity and gateway subsets add
an intra-cluster hop and special node role.

## D3: Single custom Go agent embedding scionproto libraries (Approach C)

Rejected: (A) glueing stock `scion-daemon`/`scion-ip-gateway` binaries with
shell — breaks on dynamic per-node prefix advertisement, which stock SIG's
static config model handles poorly; (B) operator orchestrating stock
components — fights the same static-config model from outside. Cost accepted:
we track scionproto internals (pinned v0.15.1; `private/` packages are not
API-stable; isolated to two files: `internal/agent/sig/sig.go`,
`internal/agent/daemonapi/daemonapi.go`).

## D4: Operator from day one, OpenShift-first

Originally designed as a plain DaemonSet + Helm (production-grade ≠ operator);
the project owner chose an operator-based architecture from the start.
`ScionNetwork` cluster-scoped singleton `cluster` (OpenShift `Network`/`Proxy`
convention), CEL-validated, status conditions ClusterOperator-style. The
OpenShift/vanilla divergence is isolated to SCC management (gated on API
discovery); PSA `privileged` namespace labels cover vanilla.

## D5: No install-config/MachineConfig/CNI integration

Day-2 add-on only. SCION attachment is not needed at install time; nothing is
kernel-level; OVN-K is untouched (host routes + own tun only). "SCION from
day 0" is GitOps applying the operator post-install.

## D6: Standalone connector; daemon gRPC API as opt-in extra

The gateway uses `pkg/daemon.NewStandaloneConnector` in-process (no gRPC hop).
The standard sciond API on `127.0.0.1:30255` for node-local SCION-native apps
is a separate opt-in module (`daemonapi`, mechanical port of upstream daemon
main), enabled by default via `SCION_ENABLE_DAEMON_API`.

## D7: Guardrails as SGRP accept-policy prefix subtraction

The gateway routing-policy language has no "except" syntax, so accepted
remote ranges are split around cluster, service, user-forbidden, and
node-to-AS underlay CIDRs. The generated rules are validated with the upstream
parser; route programming remains inside `gateway.Run`. IPv4-only by design:
IPv6 cluster/service ranges are omitted from this IPv4 policy. An empty
forbidden set is rejected fail-safe.

## D8: AS-side auto-registration via pluggable registrar

Inbound requires each node's SIG to appear in the AS topology (`sigs`). The
operator's registrar controller reconciles the full desired SIG set through a
backend: `manual` (status-published runbook), `http` (our `scion-registrar`
service next to the OSS control service — patches `topology.json`, SIGHUP
reload), `anapaya` (stub, `ErrNotImplemented`; future: Appliance Management
API). The spec's heartbeat-expiry idea was replaced by full-set
reconciliation on every sync (simpler; unclean *cluster* removal still leaves
stale entries — manual empty-PUT cleanup documented). Long-term fix: upstream
TTL/heartbeat SIG self-registration proposal (drafted in
`as-registration.md`, not yet filed).

## D9: TRC pinning against pin-file content

Original plan compared fetched TRCs to the cached copy (trust-on-first-use
hole). Changed: if a pin exists (Secret mounted at
`<stateDir>/pinned-trcs/`), the fetched TRC must equal the pin bytes; whole
fetch fails before any write otherwise.

## D10: ScionNetwork must not own the scion-system Namespace

Caught in final integration review: the operator Deployment lives in
scion-system; an ownerRef would have GC-deleted the namespace (and the
operator itself) on CR deletion. The namespace lifecycle belongs to the
deploy manifests; the controller only creates-if-absent and merges PSA
labels.

## D11: Readiness semantics

Agent readyz = bootstrap fetched AND gateway constructed (OnUp callback fires
after connector + Gateway construction, before the blocking Run). Honest
limitation: tun creation happens inside `gateway.Run`, which has no readiness
hook — full data-plane readiness needs upstream support.

## D12: Registrar sync failures degrade, never break the data plane

Registrar errors set `Degraded/RegistrarSyncFailed` (non-manual backends
only; unready agents take reason precedence), publish `lastError`, and
requeue after 1m — the reconcile itself succeeds and existing connectivity is
untouched. `registeredNodes`/`lastSyncTime` advance only on success;
`desiredSIGs` is always published.

## D13: Verified-upstream-facts discipline

Every assumption about scionproto/bootstrapper behavior was verified against
pinned sources during implementation, and several plan assumptions were
corrected as a result: bootstrapper TRC endpoint has a `/blob` suffix and
uppercase filenames; `dhcpv4.OptionWWWServer` is actually
`OptionDefaultWorldWideWebServer`; `gateway.Run` self-triggers its initial
policy load (blocking send on `ConfigReloadTrigger`); `ConfigVersion` in the
traffic policy is parsed and ignored at v0.15.1; control service reloads
topology on SIGHUP. Each is cited at the point of use in code comments.

## D14: Border router stays in the AS infrastructure

Only the endhost stack (bootstrap, daemon, SIG) moves into the node. The AS
border router — inter-AS data-plane forwarding — remains AS-side: nodes
speak SCION only as plain UDP to their local BR. Moving the BR into nodes
(cilion-style node-as-router) would make the cluster an AS attachment point
(inter-AS links, neighbor-allocated interface IDs, beaconing, transit
failure domains) — rejected to keep nodes plain endhosts. Trade-off
accepted: the AS BR(s) are a shared hairpin for all cluster ↔ SCION traffic;
ASes scale this by running multiple BRs. See research.md "Where the border
router fits".

## D15: Destination-only OVN local-gateway routing without source NAT

The live OpenShift run disproved the assumption that shared-gateway pod egress
consults host routes. Transparent pod egress therefore requires
`routingViaHost: true`; the operator observes this prerequisite but never
mutates the cluster-wide Network configuration. The observation is
non-blocking by design: probe failures (missing APIs, missing RBAC,
transient apiserver errors) degrade to an Unknown condition and a periodic
re-check instead of stopping data-plane reconciliation.

The existing SCION gateway remains the sole route publisher. Only prefixes
learned through healthy SGRP sessions select `scion0`; all other traffic keeps
its normal route. The agent allocates no egress identity and installs no SNAT:
the source chosen by the kernel survives encapsulation. On OVN-K, preserving
pod sources requires an accepted default-network `PodNetwork`
`RouteAdvertisements` configuration. Node-to-AS transport networks must be
listed in `acceptPolicy.underlayCIDRs` so discovery, registration, and SCION
control traffic cannot recursively enter the tunnel.

Rejected: APBER, because it redirects all external traffic for selected
namespaces rather than matching learned SCION destinations; BGP redistribution
of learned SCION routes, because it adds a second routing authority and FRR
coupling; eBPF/TC, because it duplicates routing policy below the kernel route
table.

## D16: Metrics are authenticated HTTPS everywhere, with in-agent auth

The agent validates scrapers itself — TokenReview then a
SubjectAccessReview for `get` on the `/metrics` non-resource URL
(kube-rbac-proxy semantics, small positive-decision cache) — and serves
TLS unconditionally. The certificate source is service-ca on OpenShift
(annotated Service) and an operator-issued, rotated self-signed CA
elsewhere, published in the `scion-node-agent-metrics-ca` ConfigMap for
scrapers to pin. The operator never rotates a valid certificate it did
not issue, so it cannot fight service-ca. Probes stay unauthenticated
(kubelet must not depend on the apiserver), and when TLS material is
expected but absent the agent stays unready rather than falling back to
plaintext. Rejected: a kube-rbac-proxy sidecar (extra image, extra
hostNetwork port, SCC churn) and plaintext-with-auth (bearer tokens in
cleartext are worse than no auth).

## D17: CNI-IPAM sources outrank node.spec.podCIDR, and are refreshed

Calico (BlockAffinity) and Cilium cluster-pool (CiliumNode) allocate pod
prefixes independently of the node-CIDR allocator, which may still
populate `node.spec.podCIDR` with a value the CNI ignores. Advertising
that stale value misroutes all return traffic, so CNI-IPAM sources take
priority whenever their API exists. Both allocate dynamically (a node's
first block appears with its first pod; blocks grow), so agents on such
clusters re-resolve and re-advertise every 30 seconds, and the
nothing-to-advertise startup guardrail becomes a warning there instead
of an error. A missing-CRD probe is deliberately a List, so "not this
CNI" (NotFound) is distinguishable from "CNI object not created yet"
(empty result → stay in refresh mode).

## D18: Masquerade exemptions are the cluster admin's job; the operator hints

Transparent egress on vanilla Kubernetes requires the CNI to exempt
remote-AS prefixes from pod-egress masquerade (kindnet: masq-agent
RETURN rule; Calico: disabled IPPool with `disableBGPExport` — without
the export opt-out BIRD advertises the pool and outranks the SCION
route; Cilium: BPF ip-masq-agent `nonMasqueradeCIDRs`). The exempted
prefixes are learned dynamically over SGRP and the exemptions are not
observable through any API, so the operator cannot verify them: the
platform condition stays honestly Unknown on vanilla, enriched with a
best-effort CNI detection naming the exact recipe to check. Rejected:
mutating CNI configuration from the operator (violates the D15
non-mutation principle) and reporting fake ConditionTrue.
