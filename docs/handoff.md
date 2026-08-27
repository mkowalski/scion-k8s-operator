# Project Handoff — SCION on OpenShift / Kubernetes

Purpose: complete state snapshot so any agent or human can continue this
work with zero prior conversation history. Last updated: **2026-08-27**.

> **Status: PARKED (planned ~1 month, until late September 2026).**
> Everything is committed, pushed, and green in CI. No work is in flight;
> no uncommitted state exists in any repo. Start at "Picking this up
> again" below.

## What this project is

Make Kubernetes/OpenShift nodes first-class SCION endhosts with
transparent, bidirectional IP-over-SCION, via an operator managing a
per-node agent. Start with the spec
(`superpowers/specs/2026-07-23-scion-k8s-operator-design.md`, including §9
as-built deviations) and `research.md`; the reasoning behind every major
choice is in `decisions.md` (D1–D18); open problems in `known-gaps.md`.

## State summary (what happened since 2026-08-20)

Chronological, all on `main`, all validated before merge:

1. **Deep-review hardening** (2026-08-25): fail-closed validation of spec
   CIDRs, non-blocking platform probe (Forbidden/transient errors degrade
   to a condition instead of stopping the data plane), unified registrar
   timeouts derived from one `ReloadTimeout` anchor, `advertisement.nodeIP`
   default flipped to `false` (routing-loop safety), CEL `isCIDR()`
   admission validation, nodeIP/underlay overlap refusal, authenticated
   HTTPS metrics (in-agent TokenReview/SAR, kube-rbac-proxy semantics),
   5-minute unready grace for Degraded, registrar TLS support
   (`--tls-cert/--tls-key` + `ca.crt` secret key for pinning).
2. **CI** (2026-08-25): `.github/workflows/ci.yaml` — build/vet/gofmt, unit
   tests, envtest suite, discovery integration test, OLM bundle-check,
   three image builds, and a three-way kind e2e matrix (see below).
3. **Fresh live e2e on ostest** (2026-08-26): full flow re-validated after
   a topology re-key; two agent bugs found and fixed — self-healing daemon
   caches on AS trust re-bootstrap (stale hostPath trust DB previously
   crash-looped all agents), and IPv4-only SGRP advertisement (dual-stack
   IPv6 pod CIDRs were leaking into advertisements).
4. **Upstream issue filed**:
   [scionproto/scion#4977](https://github.com/scionproto/scion/issues/4977)
   (`InsertTRC` returns a raw SQLite constraint error instead of a typed
   conflict). Record + verified reproducer: `docs/upstream-inserttrc-issue.md`.
   A fix approach was drafted; the upstream team handles it from here.
   Downstream follow-up marker: `TODO(scionproto/scion#4977)` in
   `internal/agent/daemonapi/daemonapi.go`.
5. **Vanilla Kubernetes support** (2026-08-27): generic forbidden-CIDR
   derivation (node `spec.podCIDRs` + ServiceCIDR API + Calico IPPools +
   CiliumNodes, sorted for determinism), CNI-IPAM pod-CIDR discovery
   (Calico BlockAffinity, Cilium CiliumNode) with a 30s advertisement
   refresh, a fail-loud guardrail when nothing is advertisable,
   operator-issued metrics serving certificates where service-ca is absent
   (CA published in the `scion-node-agent-metrics-ca` ConfigMap), and a
   CNI-aware `PlatformUnverified` message. All continuously validated by
   the kind e2e on **kindnetd, Calico, and Cilium**.

## Repository inventory

| Repo | Branch | State |
|---|---|---|
| github.com/mkowalski/scion-k8s-operator | `main` | Everything above; SCION v0.15.1. Operator, agent, registrar, CRD, manifests, OLM bundle, unit/envtest/integration tests, kind e2e (three CNIs), OpenShift e2e suite, CI, docs. Working tree clean, CI green at parking time. |
| github.com/mkowalski/metal3-dev-scripts (fork of openshift-metal3/dev-scripts) | `scion-topology` | Commit `db54f11`: ten-container SCION v0.15.1 topology, underlay-bypass guard, TCP target, discovery, registrar, AS-control policy routing. Checkout on metal-u15: `/root/tmp/metal3-dev-scripts`. Not proposed upstream yet. |
| github.com/mkowalski/scion (fork of scionproto/scion) | `fix-sig-clearsession-panic` | Merged upstream as scionproto/scion#4954, shipped in v0.15.1. Fork is done; keep only for history. |

## CI (github.com/mkowalski/scion-k8s-operator → Actions)

Every push/PR runs: gofmt + vet + build; unit tests; the controller
envtest suite; `test/integration/discovery_test.sh`; `make bundle-check`;
agent/operator/registrar image builds; and `test/e2e/kind/run.sh` as a
matrix over `CNI=kindnet|calico|cilium` (~7–9 min per leg). The kind e2e
is fully self-contained (builds the SCION AS from `Dockerfile.scion-as`
and the operator from the working tree) — it is the fastest way to verify
a change end to end without touching the live environment.

Local kind e2e on the hypervisor (podman):

```sh
CONTAINER_ENGINE=podman KIND_EXPERIMENTAL_PROVIDER=podman \
    [CNI=calico|cilium] [KEEP=1] ./test/e2e/kind/run.sh
```

## Live environment: metal-u15 (ssh root@metal-u15, key auth)

- Hypervisor running a dev-scripts OpenShift 5.x nightly cluster `ostest`
  (3 masters + 2 workers, OVN-K, RHCOS 10.2). KUBECONFIG:
  `/root/dev-scripts/ocp/ostest/auth/kubeconfig`. Real dev-scripts config:
  `/root/dev-scripts/config_root.sh`, `WORKING_DIR=/mnt/nvme0n1p1/dev-scripts/`.
  DO NOT rerun the dev-scripts pipeline; the cluster is in use.
- Cluster state at parking: operator **undeployed** (`scion-system` absent,
  registrar set empty, no `scion0` on nodes). The OVN prerequisites remain
  configured: `routingViaHost: true`, `routeAdvertisements: Enabled`, an
  accepted default `RouteAdvertisements`.
- **SCION topology LEFT RUNNING**, re-created fresh on 2026-08-26 into the
  real `WORKING_DIR` (the older `/tmp/scion-t4*` sandbox from the first
  validation round is obsolete — ignore it). Ten containers:
  scion-cs-a/br-a/dispatcher-a, scion-cs-b/br-b/daemon-b/sig-b,
  scion-remote-echo, scion-discovery, scion-registrar. Image
  `localhost/scion-infra:v0.15.1` (built locally, never pushed).
  - Re-run / teardown (from the fork checkout, config must be the real one):
    `cd /root/dev-scripts && CONFIG=/root/dev-scripts/config_root.sh \
       /root/tmp/metal3-dev-scripts/scion/configure_scion_as.sh` (or
    `cleanup_scion_as.sh`). Trust material is re-minted on every configure
    run — the agent's trust-cache self-heal (see state summary #3) makes
    this safe for deployed agents.
  - Handoff values: `DISCOVERY_URL=http://192.168.111.1:8041`,
    `REGISTRAR_URL=http://192.168.111.1:8642`,
    `REGISTRAR_TOKEN=$(cat /mnt/nvme0n1p1/dev-scripts/scion/token)`,
    `REMOTE_ISD_AS=1-ff00:0:111`, `REMOTE_PING_IP=192.168.100.1`,
    `UNDERLAY_CIDR=192.168.111.0/24`, `REMOTE_SSH=local` on the hypervisor.
- Latest validated images (2026-08-26 live run):
  `quay.io/mkowalski/scion-operator:main-20260826` and
  `quay.io/mkowalski/scion-node-agent:main-20260826-2`. These predate the
  vanilla/Cilium/metrics-TLS work — **build fresh images from `main`
  before the next live run** (`make images`, push, point
  `config/manifests/operator.yaml` at the tag as the e2e README describes).

Full OpenShift e2e (run on the hypervisor, values above):

```sh
KUBECONFIG=/root/dev-scripts/ocp/ostest/auth/kubeconfig \
DISCOVERY_URL=... REMOTE_ISD_AS=... REGISTRAR_URL=... REGISTRAR_TOKEN=... \
REMOTE_PING_IP=192.168.100.1 REMOTE_SSH=local ./test/e2e/e2e_test.sh
```

## Validation status (what has actually been proven)

- **OpenShift (2026-08-26, five-node ostest, live)**: full suite — deploy,
  5/5 agents, path-conclusive dataplane with preserved pod sources both
  directions (multi-replica workload across nodes), registrar
  registration, agent churn, remote-SIG churn (upstream #4954 fix holds),
  authenticated metrics (401/200/403 + service-ca TLS), clean undeploy.
  Conditions (incl. the 5m Degraded grace) behaved as designed.
- **Vanilla Kubernetes (continuously, in CI since 2026-08-27)**: the kind
  e2e proves per CNI — operator/agent deploy without OpenShift APIs,
  generic deny-list derivation, TCP egress with preserved pod source
  (path-conclusive via remote access log + nft tun-only guard), inbound
  remote→pod through the tunnel, metrics HTTPS/auth with the
  operator-issued CA, deregistration and tun removal on delete. The
  Calico and Cilium legs use pod prefixes disjoint from
  `node.spec.podCIDR`, so their passing inbound tests prove the CNI-IPAM
  discovery path specifically.

## Parked threads (not lost, deliberately not in flight)

- **Productization / customer discovery**: explored and consciously set
  aside (2026-08-26). Conclusion: no existing tracking or customers in
  any internal system; the realistic route is a partner-led pilot with
  the commercial SCION vendor into its existing install base (Swiss
  financial sector). Internal contact leads and account specifics were
  identified via internal-only discovery and are intentionally **not**
  recorded in this public repo — re-run the discovery (Jira/Slack/Gmail
  search for SCION/Anapaya/SSFN) or ask the repo owner.
- **Upstream #4977**: owned by the scionproto team. When a typed conflict
  error ships, narrow the `ErrWriteFailed` match in
  `internal/agent/daemonapi` (TODO marker in place).
- **TTL/heartbeat SIG self-registration proposal**: still unfiled; draft
  paragraph in `as-registration.md`.
- **dev-scripts `scion-topology` upstream PR**: still undecided.

## Picking this up again (suggested order)

1. Re-verify baseline: CI green on `main`; run one local kind e2e leg;
   optionally the full ostest suite with freshly built images (see above —
   the last live-validated images predate the vanilla work).
2. **API group rename** (`scion.mkowalski.github.io` is a placeholder):
   needs a naming decision first; breaking CRD change, so do it before
   anything external consumes v1alpha1. Cheapest now, more expensive
   every week.
3. `known-gaps.md` follow-ups in priority order — top items: scoped
   SELinux instead of the privileged agent, registrar TLS dogfooding in
   the dev/kind topologies (feature shipped, topologies still run
   plaintext with a warning), IPv6 dataplane, Anapaya registrar backend.
4. Parked threads above, as appetite allows.

## Conventions used throughout

- Commits: trailer `Assisted-By: <model name>`; public GitHub/Jira/Slack
  content carries an AI-assistance attribution note.
- scionproto pinned at v0.15.1 everywhere (operator go.mod, dev-scripts
  image, kind e2e image); bump deliberately and together — `private/`
  package churn lands in the two isolation files (`internal/agent/sig`,
  `internal/agent/daemonapi`).
- All specs/plans live under `docs/superpowers/{specs,plans}/` in whichever
  repo owns the implementation.
- Never mutate cluster-wide network config from the operator; observe and
  report (D15/D18). Fail closed on security-relevant input; degrade to
  conditions for informational probes.
