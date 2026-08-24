# Project Handoff — SCION on OpenShift

Purpose: complete state snapshot so any agent or human can continue this
work with zero prior conversation history. Last updated: 2026-08-20.

## What this project is

Make OpenShift nodes first-class SCION endhosts with transparent,
bidirectional IP-over-SCION, via a Kubernetes operator managing a per-node
agent. Start with the spec (`superpowers/specs/2026-07-23-scion-k8s-operator-design.md`,
including §9 as-built deviations) and `research.md`; the reasoning behind
every major choice is in `decisions.md`; open problems in `known-gaps.md`.

## Repository inventory

| Repo | Branch | State |
|---|---|---|
| github.com/mkowalski/scion-k8s-operator | `main` | Source-preserving OVN-K egress and hardened live e2e complete (as of 2026-08-21, including post-review hardening: fail-closed CIDR validation, non-blocking platform probe, unified registrar timeouts, `nodeIP` default off); SCION v0.15.1. Operator, agent, registrar, CRD, manifests, OLM bundle, tests, docs. |
| github.com/mkowalski/metal3-dev-scripts (fork of openshift-metal3/dev-scripts) | `scion-topology` | Commit `b3a9137`: ten-container SCION v0.15.1 topology, underlay-bypass guard, TCP target, discovery, registrar, and AS-control policy routing. Fully validated by the five-node e2e. Not proposed upstream yet. |
| github.com/mkowalski/scion (fork of scionproto/scion) | `fix-sig-clearsession-panic` | Fix merged upstream as scionproto/scion#4954 and released in v0.15.1. |

Upstream fix status:
- **Issue** [scionproto/scion#4953](https://github.com/scionproto/scion/issues/4953)
  is closed.
- **PR** [scionproto/scion#4954](https://github.com/scionproto/scion/pull/4954)
  was approved and merged on 2026-08-06. The fix shipped in
  [SCION v0.15.1](https://github.com/scionproto/scion/releases/tag/v0.15.1).

## Live environment: metal-u15 (ssh root@metal-u15, key auth)

- Hypervisor running a dev-scripts OpenShift 5.x nightly cluster "ostest"
  (3 masters + 2 workers, OVN-K, RHCOS 10.2). KUBECONFIG:
  `/root/dev-scripts/ocp/ostest/auth/kubeconfig`. Real dev-scripts config:
  `/root/dev-scripts/config_root.sh`, `WORKING_DIR=/mnt/nvme0n1p1/dev-scripts/`.
  DO NOT rerun the dev-scripts pipeline; the cluster is in use.
- **SCION topology LEFT RUNNING** in a sandbox, deliberately separate from
  the real WORKING_DIR:
  - Repo copy: `/tmp/scion-t4-repo` (rsync of the dev-scripts fork branch)
  - `WORKING_DIR=/tmp/scion-t4`, config `/tmp/scion-t4-config.sh`
    (minimal: WORKING_DIR, SSH_PUB_KEY=dummy, OPENSHIFT_RELEASE_IMAGE pinned,
    IP_STACK=v4 — required, see the EXTERNAL_SUBNET_V4 guard)
  - Re-run: `ssh metal-u15 'cd /tmp/scion-t4-repo && CONFIG=/tmp/scion-t4-config.sh scion/configure_scion_as.sh'`
    (must run from repo root); teardown: `scion/cleanup_scion_as.sh` same way.
  - 10 containers: scion-cs-a, scion-br-a, scion-dispatcher-a, scion-cs-b,
    scion-br-b, scion-daemon-b, scion-sig-b, scion-remote-echo,
    scion-discovery, scion-registrar. Image `localhost/scion-infra:v0.15.1`
    (built locally; NOT pushed anywhere by design).
  - Handoff values: `DISCOVERY_URL=http://192.168.111.1:8041`,
    `REGISTRAR_URL=http://192.168.111.1:8642`,
    `REGISTRAR_TOKEN=$(ssh metal-u15 cat /tmp/scion-t4/scion/token)`,
    `REMOTE_ISD_AS=1-ff00:0:111`, `REMOTE_PING_IP=192.168.100.1`,
    `UNDERLAY_CIDR=192.168.111.0/24`, `REMOTE_SSH=local` when the suite runs
    on the hypervisor.
- Final validation images:
  `quay.io/mkowalski/scion-operator:source-preserving-20260820-5` and
  `quay.io/mkowalski/scion-node-agent:source-preserving-20260820-5`. Registry
  trust and pull credentials are already configured on the cluster.

## Validation status (what has actually been proven)

- The hardened full operator e2e suite passed live on the five-node `ostest`
  cluster on 2026-08-20: deploy, 5/5 agents Ready, path-conclusive dataplane,
  registrar registration, replacement-pod churn, and clean undeploy.
- OVN-K used `routingViaHost: true` plus an accepted default-network
  `PodNetwork` `RouteAdvertisements` resource. The selected-node route for
  `192.168.100.1` used `scion0`; a non-SCION control route did not.
- The remote `inet scion-e2e` guard blocked direct underlay delivery. Captures
  on `sigb` observed pod source `10.128.2.49` and the node route-selected host
  source `10.128.2.2` unchanged after decapsulation. TCP and bidirectional ICMP
  passed. The agent adds no SNAT or egress identity.
- Node-IP `/32` advertisement remains disabled in e2e because routing the
  SCION underlay address into the tunnel creates a loop.
- `acceptPolicy.underlayCIDRs` excluded `192.168.111.0/24` from learned routes;
  the AS test topology source-routed its control-service replies over the
  underlay, keeping discovery, registrar sync, and finalizer cleanup reachable.

## Suggested next steps (priority order)

1. Decide on an upstream PR for the dev-scripts `scion-topology` branch.
2. CI for scion-k8s-operator (none exists; `.github/workflows` empty):
   build, vet, unit+envtest, discovery integration test, bundle-check.
3. Remaining items in `known-gaps.md` (API group placeholder, nonroot agent
   TODO, SA/pull-secret GC issue, registrar TLS, IPv6, upstream
   TTL/heartbeat SIG-registration proposal — draft in `as-registration.md`).

## Conventions used throughout

- Commits: `git commit -s` + trailer `Assisted-By: <model name>`; public
  GitHub/Jira/Slack content carries an AI-assistance attribution note.
- scionproto pinned at v0.15.1 everywhere (operator go.mod, dev-scripts
  image build); bump deliberately and together.
- All specs/plans live under `docs/superpowers/{specs,plans}/` in whichever
  repo owns the implementation.
