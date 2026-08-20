# OVN-Kubernetes Source-Preserving SCION Egress Plan

> **Execution:** implement tasks in order. The operator observes platform
> prerequisites but never mutates cluster-wide OVN configuration.

**Goal:** Route only prefixes learned through SGRP into the node-local
`scion0` tunnel while preserving the original pod or host source address.

**Decision:** Use the Linux routes already published by the embedded SCION-IP
gateway as the only traffic selector. Require OVN-Kubernetes
`routingViaHost: true` so pod traffic reaches those host routes. Do not add an
egress address pool, per-node allocations, source NAT, policy routing, APBER,
eBPF, or direct OVN database changes.

**Traffic model:** [`drawings/ovn-scion-traffic.svg`](../../../drawings/ovn-scion-traffic.svg)

## Preconditions

OpenShift OVN-Kubernetes must provide both:

1. `spec.defaultNetwork.ovnKubernetesConfig.gatewayConfig.routingViaHost: true`,
   which sends pod egress through `ovn-k8s-mp0` and the host routing table.
2. `spec.defaultNetwork.ovnKubernetesConfig.routeAdvertisements: Enabled` plus
   an accepted `RouteAdvertisements` resource advertising `PodNetwork` for the
   default network. OVN-Kubernetes otherwise masquerades pod traffic in local
   gateway mode. Advertising the pod network suppresses that masquerade and
   preserves pod source addresses.

The operator reports an existing `Available=False` / `Degraded=True` state when
an OpenShift cluster does not satisfy these prerequisites. It does not create a
`RouteAdvertisements` resource or modify the cluster Network object because
those are cluster-wide routing decisions.

## Invariants

1. The SCION gateway installs host routes only for prefixes learned from healthy
   SGRP sessions. Those routes select `scion0`; all other destinations retain
   their existing routes.
2. The agent does not perform SNAT. Pod and host source addresses are unchanged
   across encapsulation and visible unchanged after remote SIG decapsulation.
3. Pod CIDRs are advertised through SGRP for return traffic. Node IP
   advertisement remains explicit because it is unsafe when the same address is
   also used by the SCION underlay.
4. The operator owns no OVN objects and does not modify `br-ex`, OVS flows,
   nftables, policy-routing rules, or node annotations.
5. Existing inbound SCION behavior and node-local SCION daemon behavior remain
   unchanged.

## Non-goals

- Supporting transparent pod egress in OVN shared-gateway mode.
- Automatically enabling local gateway mode or RouteAdvertisements.
- Replacing OVN's platform-managed source-preservation configuration.
- Adding a second routing authority for learned SCION prefixes.
- Adding egress IP allocation, SNAT, APBER, BGP redistribution, or eBPF/TC.

---

### Task 1: Add read-only OVN prerequisite detection

**Files:**
- Create: `internal/operator/controller/platform.go`
- Create: `internal/operator/controller/platform_test.go`
- Modify: `internal/operator/controller/scionnetwork_controller.go`
- Modify: `internal/operator/controller/registrar_sync.go`
- Modify: corresponding controller tests
- Modify: `config/manifests/rbac.yaml`
- Regenerate: `bundle/manifests/scion-operator.clusterserviceversion.yaml`

- [x] Read `Network.operator.openshift.io/cluster` as unstructured data.
- [x] Return `HostRoutingDisabled` when the default network is OVN-Kubernetes
  and `routingViaHost` is absent or false.
- [x] When host routing is enabled, require
  `routeAdvertisements: Enabled` and at least one accepted
  `RouteAdvertisements.k8s.ovn.org` object containing `PodNetwork` and a
  `DefaultNetwork` selector. Return `SourcePreservationDisabled` otherwise.
- [x] Return `PlatformUnverified` without degrading on non-OpenShift or non-OVN
  clusters, where CNI behavior cannot be inferred from these APIs.
- [x] Fold a false platform result into the existing `Available` and `Degraded`
  conditions. Do not add egress-specific fields to the public API.
- [x] Add read-only RBAC for `networks.operator.openshift.io` and
  `routeadvertisements.k8s.ovn.org`.
- [x] Test true, false, missing, malformed, and unknown-platform cases.

**Acceptance:** shared-gateway OpenShift reports `Available=False` and
`Degraded=True/HostRoutingDisabled`; the current local-gateway BGP-demo cluster
is accepted without creating or changing any platform resource.

---

### Task 2: Preserve the existing destination-only data path

**Files:**
- Verify: `internal/agent/sig/sig.go`
- Verify: `internal/agent/policy/policy.go`
- Modify tests only if coverage is missing

- [x] Keep `gateway.Gateway.RouteSourceIPv4` unset. Host applications retain
  the source selected by the kernel.
- [x] Keep the upstream SGRP route publisher as the sole writer of routes to
  `scion0`; do not add broad routes or policy rules.
- [x] Keep traffic-policy `Nets` empty so it does not install covering routes;
  accepted SGRP prefixes define the exact kernel routes.
- [x] Keep pod CIDR and explicit node IP advertisements unchanged. Do not add
  an egress identity to `AdvertisedNets`.
- [x] Retain the existing per-interface IPv4 forwarding setup required for
  inbound packets leaving `scion0` toward pods.

**Acceptance:** unit tests confirm generated policy contains only configured
local advertisements and accepted remote ranges. Runtime route inspection shows
only learned SGRP prefixes using `scion0`.

---

### Task 3: Remove the superseded egress-identity design

**Files:**
- Do not add fields to `api/v1alpha1/scionnetwork_types.go`
- Do not add an agent egress package
- Do not add node-allocation reconciliation
- Keep `go.mod` free of an nftables dependency

- [x] Remove `egressIPPool`, node-to-IP status, allocation annotations, and
  allocation finalizer behavior from the implementation and generated CRDs.
- [x] Remove custom SNAT and egress-IP assignment code.
- [x] Remove egress-pool environment variables and related RBAC mutations.
- [x] Verify no `scion.mkowalski.github.io/egress-ip` annotation is read or
  written anywhere.

**Acceptance:** repository search finds no egress-pool, allocation, or custom
SNAT implementation. The only host routing changes remain those made by the
upstream SCION gateway plus the existing tun forwarding sysctl.

---

### Task 4: Make the test topology path-conclusive

**Files:**
- Modify: sibling repo `metal3-dev-scripts/scion/configure_scion_as.sh`
- Modify: sibling repo `metal3-dev-scripts/scion/cleanup_scion_as.sh`
- Modify: sibling repo `metal3-dev-scripts/common.sh`
- Modify: sibling repo `metal3-dev-scripts/config_example.sh`

- [x] Install a dedicated `inet scion-e2e` nftables input chain on the AS host
  that drops traffic to `REMOTE_PING_IP` unless it arrived on the remote SIG
  tunnel interface `sigb`.
- [x] Configure the remote SIG to accept the cluster pod CIDRs, not the machine
  underlay subnet.
- [x] Add a small TCP endpoint bound to `REMOTE_PING_IP` so the test covers TCP
  as well as ICMP.
- [x] Permit the configured cluster pod CIDRs through the AS host firewall so
  their original source addresses remain usable.
- [x] Remove the dedicated guard and endpoint during topology cleanup.
- [x] Reset SCION state volumes before regenerating trust material, preventing
  stale-TRC database conflicts on repeated configure runs.

**Acceptance:** direct node-underlay traffic to `REMOTE_PING_IP` fails, while a
packet entering through `sigb` is accepted. Re-running topology configuration is
idempotent on SCION v0.15.1.

---

### Task 5: Harden end-to-end source and route assertions

**Files:**
- Modify: `test/e2e/e2e_test.sh`
- Modify: `test/e2e/README.md`

- [x] Fail before configuration unless `routingViaHost` is true.
- [x] Fail unless route advertisements are enabled and an accepted default
  `PodNetwork` advertisement exists.
- [x] Schedule the test pod, identify its actual node, and assert the learned
  remote prefix resolves to `dev scion0` in that node's host routing table.
- [x] Capture the decapsulated ICMP request on remote `sigb`; assert its source
  exactly equals the pod IP.
- [x] Exercise a TCP request to the remote test endpoint through the same
  underlay-blocking guard.
- [x] Generate host-originated traffic and capture it on remote `sigb`; assert
  the source equals the address selected by the node's route before
  encapsulation.
- [x] Preserve the existing remote-to-pod test and assert the echo reply source
  remains the pod IP.
- [x] During churn, wait for a replacement pod UID rather than stale DaemonSet
  counts, then repeat route and connectivity checks.
- [x] During undeploy, assert `scion0` and learned routes disappear. There are no
  allocation annotations or nftables objects to clean.

**Acceptance:** the suite fails in shared-gateway mode, fails when the underlay
bypass guard is absent, and passes only when packets to the learned remote
prefix traverse `scion0` with unchanged pod and host source addresses.

---

### Task 6: Update architecture and operational documentation

**Files:**
- Modify: `README.md`
- Modify: `drawings/ovn-scion-traffic.svg`
- Modify: `docs/install.md`
- Modify: `docs/decisions.md`
- Modify: `docs/research.md`
- Modify: `docs/known-gaps.md`
- Modify: `docs/handoff.md`
- Modify: `docs/superpowers/specs/2026-07-23-scion-k8s-operator-design.md`

- [x] Replace the egress-pool/SNAT design with destination-only host routing.
- [x] Document both source-preservation prerequisites and their operational
  tradeoffs.
- [x] Keep the OVN gap open until the hardened live test succeeds.
- [x] After successful live validation, record exact evidence and retire the
  false-positive warning.

---

### Task 7: Verification and live rollout

Local checks:

```sh
make test
make lint
make build
make bundle
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path 1.34.x)" \
  go test ./internal/operator/controller -count=1
bash -n test/e2e/e2e_test.sh
```

Live sequence:

1. Record the existing Network operator configuration.
2. Rebuild the SCION v0.15.1 development topology with the underlay guard.
3. Build and push uniquely tagged operator and agent images.
4. Apply manifests and deploy those exact image tags.
5. In an approved maintenance window, set `routingViaHost: true` and wait for
   the Network ClusterOperator and `ovnkube-node` rollout to stabilize.
6. Run deploy/configure, readiness, dataplane, registration, and churn checks.
7. Capture local `scion0`, node underlay, and remote `sigb` traffic for both pod
   and host sources.
8. Run undeploy cleanup checks.
9. Leave `routingViaHost: true` only when explicitly requested by the cluster
   owner; otherwise restore the recorded value.

**Final acceptance criteria:**

1. Only destinations represented by learned SGRP routes select `scion0`.
2. Pod source IP is unchanged when observed after remote SIG decapsulation.
3. Host source selected by the pre-encapsulation node route is unchanged when
   observed after remote SIG decapsulation.
4. Non-SCION destinations continue to use the normal host uplink.
5. Direct underlay access to the remote test target is blocked.
6. Remote-initiated pod traffic remains bidirectional.
7. Agent churn restores the same route behavior without leaked host state.
8. Unit, race, vet, build, bundle, image, and live e2e checks pass.

## Validation result — 2026-08-20

The full suite passed on the five-node `ostest` OpenShift 5.0 environment with
OVN local-gateway routing and default-network `PodNetwork` route advertisements.
The path-conclusive checks observed pod source `10.128.2.49` after remote SIG
decapsulation, observed the node route-selected host source `10.128.2.2`, passed
TCP and bidirectional ICMP through `sigb`, rejected direct underlay delivery,
kept the `192.168.111.0/24` node-to-AS transport outside SCION, recovered the
learned route after agent replacement, and removed tunnel, route, and registrar
state during teardown.

## Commit boundaries

1. `operator: report unsupported OVN gateway configuration`
2. `test: prove source-preserving SCION egress path`
3. `docs: document destination-only OVN egress routing`

Each commit must be signed off and include:

```text
Assisted-By: github-copilot/gpt-5.6-sol
```
