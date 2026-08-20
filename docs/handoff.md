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
| github.com/mkowalski/scion-k8s-operator | `main` | v0.1.0 complete + live-e2e fixes merged; SCION library upgraded to v0.15.1. Operator, agent, registrar, CRD, manifests, OLM bundle, tests, docs. |
| github.com/mkowalski/metal3-dev-scripts (fork of openshift-metal3/dev-scripts) | `scion-topology` | `ENABLE_SCION_AS` feature: two-AS SCION v0.15.1 topology on the hypervisor (9 podman containers, locally-built image, templated topologies, testcrypto trust material, discovery server, registrar). Fully validated live on v0.15.0; the v0.15.1 image build and binary smoke checks pass. NOT PR'd upstream (needs explicit approval). Plan: `docs/superpowers/plans/2026-07-24-scion-topology.md` in that repo, all tasks checked. |
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
  - 9 containers: scion-cs-a, scion-br-a, scion-dispatcher-a, scion-cs-b,
    scion-br-b, scion-daemon-b, scion-sig-b, scion-discovery, scion-registrar.
    Image `localhost/scion-infra:v0.15.0` (built locally; NOT pushed anywhere
    by design).
  - Handoff values for the operator e2e:
    `DISCOVERY_URL=http://192.168.111.1:8041`,
    `REGISTRAR_URL=http://192.168.111.1:8642`,
    `REGISTRAR_TOKEN=$(ssh metal-u15 cat /tmp/scion-t4/scion/token)`,
    `REMOTE_ISD_AS=1-ff00:0:111`, `REMOTE_PING_IP=192.168.100.1`.
  - The existing v0.15.0 image can crash under heavy agent churn.
    Rebuilding the topology image with v0.15.1 picks up the merged fix.
- Operator/agent images for the cluster were built on the host and pushed to
  the dev-scripts local registry (`virthost.ostest.test.metalkube.org:5000`,
  tags `scion/scion-{operator,node-agent}:e2e`); registry CA trust +
  pull-secret linkage were configured on the cluster and left in place.

## Validation status (what has actually been proven)

- Full operator e2e suite passed live on the ostest cluster: deploy, 5/5
  agents Ready, real registrar registration of all nodes, churn, clean
  undeploy. Inbound remote→pod traffic verified BY PACKET CAPTURE to
  traverse SCION.
- **Outbound pod→remote is a FALSE POSITIVE**: OVN-K's default shared
  gateway mode (`routingViaHost: false`) bypasses host routes for pod
  egress, so pod-originated traffic reaches the remote over the plain
  underlay, not SCION. This is the biggest open design problem — candidate
  fixes: local-gateway mode, admin policy-based external routes, or a
  steering shim. See `known-gaps.md` finding 1.
- Node-IP `/32` advertisement is disabled in e2e (routing the SCION underlay
  into the tunnel created a loop); operator-side overlap guard proposed.

## Suggested next steps (priority order)

1. Design + fix the OVN-K pod-egress bypass (spec-level change; brainstorm
   before coding).
2. Decide on upstream PR for the dev-scripts `scion-topology` branch.
3. CI for scion-k8s-operator (none exists; `.github/workflows` empty):
   build, vet, unit+envtest, discovery integration test, bundle-check.
4. Remaining items in `known-gaps.md` (API group placeholder, nonroot agent
   TODO, SA/pull-secret GC issue, registrar TLS, IPv6, upstream
   TTL/heartbeat SIG-registration proposal — draft in `as-registration.md`).

## Conventions used throughout

- Commits: `git commit -s` + trailer `Assisted-By: <model name>`; public
  GitHub/Jira/Slack content carries an AI-assistance attribution note.
- scionproto pinned at v0.15.1 everywhere (operator go.mod, dev-scripts
  image build); bump deliberately and together.
- All specs/plans live under `docs/superpowers/{specs,plans}/` in whichever
  repo owns the implementation.
