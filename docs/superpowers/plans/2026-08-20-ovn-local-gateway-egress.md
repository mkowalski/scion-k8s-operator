# OVN-Kubernetes Local-Gateway SCION Egress Implementation Plan

> **Execution:** implement tasks in order. Keep the operator passive toward the
> cluster-wide OVN gateway setting: detect and report it, but never patch it.

**Goal:** Make pod-originated traffic to prefixes learned through SCION traverse
the node-local `scion0` tunnel on OVN-Kubernetes, without advertising or
source-NATing to the node's underlay address.

**Decision:** Require OVN-Kubernetes `routingViaHost: true`. A user-supplied
`spec.dataplane.egressIPPool` provides stable, operator-allocated per-node IPv4
addresses. Each agent assigns its `/32` to `scion0`, advertises it via SGRP,
sets it as the source hint for learned routes, and source-NATs only locally
originated pod flows whose selected output interface is `scion0`.

**Traffic model:** [`drawings/ovn-scion-traffic.svg`](../../../drawings/ovn-scion-traffic.svg)

**Tech stack:** Go 1.26.4, scionproto/scion v0.15.1, controller-runtime,
`vishvananda/netlink`, and a pinned `github.com/google/nftables` release for
pure-Go netfilter programming. No `nft` binary is added to the distroless agent
image.

## Invariants

1. The operator never changes `Network.operator.openshift.io/cluster`, `br-ex`,
   OVS flows, or the OVN northbound database.
2. A node keeps its egress IP while it remains selected and the address remains
   valid within the configured pool. Adding another node never renumbers
   existing nodes.
3. The egress pool never overlaps pod, service, machine/host, node-address,
   or explicitly forbidden address space. The pool is added automatically to
   the agent's forbidden-prefix set, so overlapping remote routes are rejected.
4. The nftables rule matches both the node's pod source CIDRs and
   `oifname == scion0`; ordinary pod egress is untouched.
5. Node underlay IP advertisement defaults off. The underlay carries SCION UDP
   but is not recursively advertised through SGRP.
6. Agent readiness remains false until the egress `/32`, tunnel forwarding, and
   nftables state are installed.
7. Invalid or exhausted allocation never causes partial renumbering. Existing
   valid allocations stay in place and the resource becomes Degraded.
8. Every host object created by this feature has deterministic ownership and a
   tested cleanup path.

## Deliberate non-goals

- Shared-gateway steering through APBER, direct OVN database writes, eBPF/TC,
  or BGP redistribution of learned SCION prefixes.
- IPv6 egress allocation; the current policy engine remains IPv4-only.
- Automatically changing `routingViaHost` or reverting it during uninstall.
- Preserving pod source addresses for pod-initiated connections. Remote-initiated
  connections to advertised pod CIDRs are not source-NATed on reply.

---

### Task 1: Add the API and generated artifacts

**Files:**
- Modify: `api/v1alpha1/scionnetwork_types.go`
- Modify: `api/v1alpha1/scionnetwork_types_test.go`
- Regenerate: `api/v1alpha1/zz_generated.deepcopy.go`
- Regenerate: `config/crd/scion.mkowalski.github.io_scionnetworks.yaml`
- Regenerate: `bundle/manifests/scion.mkowalski.github.io_scionnetworks.yaml`
- Modify: `config/samples/scion_v1alpha1_scionnetwork.yaml`

- [ ] Add `DataplaneSpec.EgressIPPool string` as `egressIPPool`. Keep it
  schema-optional so existing v1alpha1 resources remain readable during an
  upgrade, but document it as operationally required for transparent pod
  egress. Runtime validation remains authoritative because overlap checks need
  live cluster and Node data.
- [ ] Add `NodeSummary.EgressIPs map[string]string` as `egressIPs` for the
  observed node-to-address assignments.
- [ ] Change `AdvertisementSpec.NodeIP`'s schema default from `true` to `false`.
  This is a deliberate alpha API behavior correction after the live underlay
  loop. Update Go-side defaults in later tasks so generated and runtime
  behavior agree.
- [ ] Add API serialization/default tests for `egressIPPool`, `egressIPs`, and
  the node-IP default.
- [ ] Regenerate objects and CRDs:

  ```sh
  bin/controller-gen object paths=./api/...
  make manifests
  make bundle
  ```

- [ ] Update the sample with a clearly non-production example pool:

  ```yaml
  dataplane:
    tunName: scion0
    egressIPPool: 198.18.0.0/24
  advertisement:
    podCIDR: true
    nodeIP: false
  ```

**Acceptance:** `go test ./api/v1alpha1`, `make bundle-check`, and structural
schema validation pass. Existing resources without the new field still decode.

---

### Task 2: Implement a deterministic, stable allocator

**Files:**
- Create: `internal/operator/egress/allocator.go`
- Create: `internal/operator/egress/allocator_test.go`
- Modify: `internal/operator/controller/scionnetwork_controller.go`
- Modify: `internal/operator/controller/scionnetwork_controller_test.go`

Use the Node annotation
`scion.mkowalski.github.io/egress-ip` as persisted allocation state. Do not add
owner references to Node objects; cleanup is explicit.

- [ ] Implement a pure allocator function taking the pool, selected Nodes, and
  their current annotations. It returns the complete desired assignment or an
  error without mutating Kubernetes objects.
- [ ] Parse the pool with `net/netip`; require IPv4 unicast and reject
  unspecified, loopback, multicast, and link-local ranges.
- [ ] Reserve the network and broadcast addresses for prefixes `/30` or larger.
  Permit `/31` and `/32` only with their literal usable capacity, documented in
  tests.
- [ ] Preserve every unique, in-pool existing allocation. Resolve duplicate,
  malformed, and out-of-pool annotations deterministically by node name.
- [ ] Allocate the lowest free usable address to new selected nodes sorted by
  name. Never renumber a valid existing node to compact holes.
- [ ] Reject the full desired state if capacity is insufficient. Return the
  required and available counts in the error.
- [ ] Validate non-overlap against:
  - OpenShift cluster and service networks;
  - every selected Node pod CIDR;
  - every Node InternalIP;
  - OVN's `k8s.ovn.org/host-cidrs` annotation when present;
  - `spec.acceptPolicy.forbiddenCIDRs`.
- [ ] Append the egress pool to the forbidden CIDRs passed to all agents, so a
  remote SGRP advertisement can never install a route covering local egress
  identities.
- [ ] Patch only changed Node annotations after the full allocation validates.
  Remove the owned annotation from nodes no longer selected.
- [ ] Copy the resulting assignments into `status.nodes.egressIPs`.

**Tests:** table-driven allocator cases for stability across node addition and
removal, duplicate recovery, malformed annotations, `/32`, `/31`, normal pool
boundaries, exhaustion, each overlap source, deterministic output, and no
partial result on error. Controller fake-client tests must prove annotation
patches converge without an update loop.

**Acceptance:** adding a node cannot change another node's egress IP; restarting
the operator reconstructs the same allocation solely from Node annotations.

---

### Task 3: Detect the OVN gateway mode and report pod-egress readiness

**Files:**
- Create: `internal/operator/controller/platform.go`
- Create: `internal/operator/controller/platform_test.go`
- Modify: `internal/operator/controller/scionnetwork_controller.go`
- Modify: `internal/operator/controller/registrar_sync.go`
- Modify: corresponding controller tests
- Modify: `config/manifests/rbac.yaml`
- Regenerate: `bundle/manifests/scion-operator.clusterserviceversion.yaml`

- [ ] Read the unstructured singleton
  `Network.operator.openshift.io/cluster`. Do not import the OpenShift API Go
  module solely for this check.
- [ ] Interpret platform state as:
  - OVN plus `routingViaHost: true`: supported;
  - OVN plus false or absent: unsupported, reason `HostRoutingDisabled`;
  - API group/object absent: unknown, reason `PlatformUnverified`;
  - transient read failure: reconcile error, preserving prior status.
- [ ] Add a `PodEgressReady` condition:
  - `True/EgressConfigured` only after the pool is valid and all selected agent
    pods are Ready;
  - `False/EgressIPPoolMissing`, `False/EgressIPAllocationFailed`, or
    `False/HostRoutingDisabled` for actionable failures;
  - `Unknown/PlatformUnverified` outside OpenShift.
- [ ] On OpenShift, make `Available=False` and `Degraded=True` while
  `PodEgressReady=False`; retain native SCION and inbound service by leaving
  the DaemonSet running. Define deterministic Degraded reason precedence:
  `ApplyFailed`, `UnreadyAgents`, `EgressIPAllocationFailed`,
  `HostRoutingDisabled`, then `RegistrarSyncFailed`.
- [ ] Add operator RBAC `get` for `networks.operator.openshift.io`; add
  `patch`/`update` for core `nodes`. Keep agent RBAC at `get` on its own Node.
- [ ] Regenerate and validate the OLM bundle after RBAC changes.

**Acceptance:** tests cover true, false, absent API, absent field, malformed
object, and read-error paths. No code path writes the OpenShift Network object.

---

### Task 4: Carry the allocated identity into agent policy and routes

**Files:**
- Modify: `internal/agent/kube/node.go`
- Modify: `internal/agent/kube/node_test.go`
- Modify: `internal/agent/config/config.go`
- Modify: `internal/agent/config/config_test.go`
- Modify: `cmd/agent/main.go`
- Modify: `cmd/agent/main_test.go`
- Modify: `internal/agent/sig/sig.go`
- Modify: `internal/agent/sig/sig_test.go`

- [ ] Extend `kube.NodeInfo` with `EgressIP`. Read it from the owned Node
  annotation and validate it as IPv4. For non-cluster integration runs, add the
  explicit `SCION_EGRESS_IP` override alongside `SCION_LOCAL_PREFIXES` and
  `SCION_NODE_IP`.
- [ ] Treat a missing or malformed allocation as not ready. Retry Node reads
  with context cancellation long enough to cover the Node/DaemonSet controller
  race; log one concise waiting message rather than crash-looping immediately.
- [ ] Always append `<egressIP>/32` to `policy.Input.AdvertisedNets`; it is
  independent of `advertisement.nodeIP`.
- [ ] Change the runtime default for `SCION_ADVERTISE_NODE_IP` to `false` and
  update render/config tests.
- [ ] Add `RouteSourceIPv4 net.IP` to `sig.Params` and pass it to
  `gateway.Gateway.RouteSourceIPv4`. This makes locally generated host traffic
  choose the egress identity for routes installed by the gateway.
- [ ] Reject IPv6 egress identities explicitly rather than silently omitting
  the source hint.

**Acceptance:** policy tests show pod CIDR plus egress `/32`, never an implicit
underlay `/32`; SIG construction tests show the egress IP reaches
`RouteSourceIPv4`.

---

### Task 5: Configure `scion0` and an owned nftables SNAT atomically

**Files:**
- Create: `internal/agent/egress/egress.go`
- Create: `internal/agent/egress/egress_test.go`
- Modify: `internal/agent/sig/sig.go`
- Modify: `cmd/agent/main.go`
- Modify: `internal/agent/health/health.go`
- Modify: `internal/agent/health/health_test.go`
- Modify: `go.mod`, `go.sum`

Pin `github.com/google/nftables` explicitly after verifying the selected
release against the target RHCOS kernel. Continue using the already-present
`github.com/vishvananda/netlink` API for link/address operations.

- [ ] Replace the current forwarding-only tunnel poller with an egress
  configurator that waits for `scion0`, applies `<egressIP>/32` using
  idempotent address replacement, and enables IPv4 forwarding.
- [ ] Create a dedicated `inet scion-node-agent` table and a NAT postrouting
  base chain at priority 99, before OVN-K's local-gateway masquerade chain at
  priority 101.
- [ ] For every IPv4 pod CIDR on the node, install one rule matching both the
  source CIDR and output interface name, then `snat` to the node egress IP.
  Do not match destinations and do not add rules for non-pod sources.
- [ ] Build the complete desired nftables table in memory and commit it in one
  netlink batch. Reconciliation replaces only the operator-owned table; never
  flush global tables or chains.
- [ ] On startup, replace stale owned state left by a crash. On graceful
  shutdown, best-effort delete the owned table. A failed cleanup must not hide
  the original gateway error.
- [ ] Add `health.ComponentEgress`; `/readyz` requires bootstrap, gateway, and
  egress configuration. If address or nftables programming fails, return an
  error so the pod restarts and remains unready.
- [ ] Unit-test desired-rule construction and lifecycle using a narrow
  connection interface/fake. Tests must cover multiple pod CIDRs, custom tun
  names, idempotent replacement, cleanup, address failure, nftables failure,
  and context cancellation.

**Important behavioral test:** an outbound pod-initiated flow is SNATed to the
per-node `/32`, while a reply belonging to a remote-initiated pod connection is
not newly SNATed. This relies on conntrack's first-packet NAT semantics and must
be demonstrated in the live test, not asserted from rule text alone.

**Acceptance:** agent readiness becomes true only after the host contains the
`/32` and exactly one owned SNAT ruleset. Ordinary pod traffic whose route does
not select `scion0` is unchanged.

---

### Task 6: Complete reconciliation and lifecycle cleanup

**Files:**
- Modify: `internal/operator/controller/scionnetwork_controller.go`
- Modify: `internal/operator/controller/scionnetwork_controller_test.go`
- Modify: `internal/operator/render/render.go`
- Modify: `internal/operator/render/render_test.go`
- Modify: `config/manifests/rbac.yaml`

- [ ] Allocate and patch Node annotations before applying/updating the
  DaemonSet, minimizing the new-node startup race.
- [ ] Put `SCION_EGRESS_IP_POOL` in the DaemonSet environment even though the
  agent reads its concrete allocation from the Node. A pool change then changes
  the pod template and forces a controlled rollout after annotations are
  reconciled.
- [ ] When the pool changes, preserve addresses still valid in the new pool;
  update invalid allocations before rolling the DaemonSet.
- [ ] During ScionNetwork finalization, remove all owned Node annotations before
  releasing the existing registrar finalizer. Cleanup errors retry until the
  object can be safely finalized; do not silently leave allocation state.
- [ ] When a node becomes unselected, remove its annotation only after it no
  longer has an agent pod. This keeps the running agent's identity valid during
  DaemonSet scale-down.
- [ ] Ensure Node events caused by the operator's own annotation patches
  converge in one additional reconcile without rewriting unchanged objects.

**Acceptance:** selector changes, pool changes, node deletion, operator restart,
and ScionNetwork deletion leave neither duplicate allocations nor orphaned
annotations.

---

### Task 7: Harden the end-to-end topology against underlay false positives

**Files:**
- Modify: sibling repo `metal3-dev-scripts/scion/configure_scion_as.sh`
- Modify: sibling repo `metal3-dev-scripts/scion/cleanup_scion_as.sh`
- Modify: `test/e2e/e2e_test.sh`
- Modify: `test/e2e/README.md`

- [ ] In the dev topology, add a dedicated host nftables input table that drops
  packets to `REMOTE_PING_IP` unless they enter through the remote SIG tunnel
  interface `sigb`. Create it after the tunnel configuration is known; delete
  only that table during cleanup.
- [ ] Add `EGRESS_IP_POOL` to e2e configuration, defaulting to
  `198.18.0.0/24` only for this isolated topology.
- [ ] Make `configure` include the pool and keep `advertisement.nodeIP: false`.
- [ ] Add a preflight that requires `routingViaHost: true`; print the documented
  `oc patch` command but never execute it from the suite.
- [ ] Strengthen `assert_dataplane` to check:
  - `PodEgressReady=True`;
  - every selected Node has a unique allocation annotation;
  - `scion0` carries that `/32`;
  - the owned nftables table contains the node pod CIDR and egress IP;
  - pod-to-remote ICMP succeeds with the underlay guard active;
  - remote-to-pod ICMP succeeds and its reply retains the pod source;
  - no plain packet to `REMOTE_PING_IP` appears on the node underlay during the
    successful exchange.
- [ ] Fix `wait_agents_ready` so it waits for a new DaemonSet generation and
  new pod UIDs after churn rather than accepting stale status.
- [ ] After churn, assert the allocation is unchanged and there is exactly one
  owned nftables table.
- [ ] After undeploy, assert Node annotations and the nftables table are gone.

**Acceptance:** the outbound assertion fails in shared-gateway mode and passes
in local-gateway mode only when captures prove traversal through `scion0` and
SCION UDP. The former false-positive path is impossible.

---

### Task 8: Update user-facing documentation and decisions

**Files:**
- Modify: `README.md`
- Modify: `docs/install.md`
- Modify: `docs/decisions.md`
- Modify: `docs/research.md`
- Modify: `docs/known-gaps.md`
- Modify: `docs/handoff.md`
- Modify: `docs/superpowers/specs/2026-07-23-scion-k8s-operator-design.md`

- [ ] Replace the disproven shared-gateway traffic description with the
  local-gateway + per-node egress identity model.
- [ ] Document the cluster-wide performance/hardware-offload tradeoff from
  `routingViaHost: true`, with the exact read-only preflight and administrator
  patch command.
- [ ] Document pool sizing, exclusions, stable allocation annotations, status,
  nftables ownership, rollback order, and why underlay node IP advertisement
  defaults off.
- [ ] Update the original design's statement that Node annotations are avoided;
  the egress allocation annotation is now intentional persisted operator state.
- [ ] Mark the OVN-K pod-egress gap retired only after the live acceptance run.
  Until then the README diagram and plan remain explicitly labeled planned.

**Acceptance:** docs distinguish implemented behavior from planned behavior at
every stage; no claim of working pod egress appears before live verification.

---

### Task 9: Verification and live rollout

Run local checks first:

```sh
gofmt -w <changed-go-files>
make test
make lint
make build
make manifests
make bundle-check
podman build -f build/Dockerfile.agent -t localhost/scion-node-agent:ovn-egress .
```

- [ ] Confirm tests pass without privileged host access.
- [ ] Run the agent image in a disposable Linux network namespace or test VM to
  verify pure-Go nftables programming against the target kernel before touching
  the OpenShift cluster.
- [ ] Record the current
  `Network.operator.openshift.io/cluster.spec.defaultNetwork.ovnKubernetesConfig.gatewayConfig`
  and wait for an explicit maintenance window.
- [ ] Apply `routingViaHost: true` manually and wait for the Network operator and
  all nodes to become stable. Do not combine this platform change with operator
  deployment in the same command.
- [ ] Deploy the operator and run the hardened e2e suite against the live SCION
  topology.
- [ ] Capture evidence on both sides:
  - inner packet on local `scion0` with allocated source;
  - SCION UDP between node and local border router;
  - decapsulated packet on remote `sigb`;
  - absence of plain inner traffic on the node underlay;
  - reverse traffic restored to the pod by conntrack.
- [ ] Exercise one agent restart and one node add/remove if capacity permits.
- [ ] Run undeploy cleanup checks.
- [ ] Restore the prior gateway setting manually if the environment owner wants
  the experiment rolled back.

**Final acceptance criteria:**

1. Pod egress cannot succeed through the plain underlay test path.
2. Pod-initiated ICMP and TCP traverse SCION bidirectionally.
3. Remote-initiated traffic to an advertised pod CIDR remains bidirectional.
4. Every selected node has one stable unique egress `/32`; no node underlay IP
   is advertised by default.
5. Shared-gateway mode is detected and reported, never mutated.
6. Agent churn preserves allocation and leaves no duplicate host state.
7. Deletion removes routes, tunnel, nftables state, registrar entries, and Node
   annotations.
8. Unit, vet, build, bundle, image, and live e2e checks all pass.

## Commit boundaries

Keep changes reviewable:

1. `api: add per-node SCION egress address pool`
2. `operator: allocate stable node SCION egress addresses`
3. `operator: report OVN host-routing readiness`
4. `agent: configure SCION egress identity and SNAT`
5. `test: prove pod egress traverses SCION`
6. `docs: document OVN local-gateway traffic model`

Each commit must be signed off and include:

```text
Assisted-By: github-copilot/gpt-5.6-sol
```
