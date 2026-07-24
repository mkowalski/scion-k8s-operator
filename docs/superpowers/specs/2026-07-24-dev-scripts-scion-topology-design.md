# dev-scripts SCION Topology Feature — Design

Date: 2026-07-24
Status: Draft for review
Implementation target: fork of github.com/openshift-metal3/dev-scripts
(mkowalski/dev-scripts), feature directory `scion/`
Companion project: github.com/mkowalski/scion-k8s-operator

## 1. Goal

Let dev-scripts stand up a complete local SCION topology on the hypervisor
host — two ASes, a remote SIG, a discovery server, and the scion-registrar —
so that the virtualized OpenShift cluster's nodes can join the SCION network
as endhosts via scion-k8s-operator. This is the "Stage 0" live-test
environment for the operator: everything runs on one host, no external SCION
connectivity (no SCIONLab), no version skew (all components built from the
same pinned scionproto tag the operator embeds).

The feature follows the established dev-scripts idiom used by the FRR BGP
ToR feature (`bgp/configure_bgp_tor.sh`): env-var toggle with defaults in
`common.sh` and docs in `config_example.sh`, a feature subdirectory invoked
conditionally from `02_configure_host.sh`, privileged `--net host` podman
containers with config generated into `$WORKING_DIR/scion/`, firewalld
openings in the libvirt zone, and an idempotent cleanup script wired into
`host_cleanup.sh`.

## 2. Constraints and decisions

- **No pre-pushed images.** The SCION infrastructure image is built locally
  with `podman build` every time dev-scripts spawns the topology (layer
  cache makes repeats cheap). Nothing is pulled from quay beyond public base
  images.
- **Templated topology.** No `scion.sh topology` / Python tooling from the
  scionproto repo. Two hand-written `topology.json` templates are rendered
  with the host bridge IP and port assignments; crypto (TRCs, CA, AS certs)
  is minted by `scion-pki testcrypto` in a one-shot container.
- **Host infrastructure only.** The feature does not deploy the operator or
  agent into the cluster; it prints the handoff values (discovery URL,
  registrar endpoint/token, remote ISD-AS, remote ping IP) that
  scion-k8s-operator's `test/e2e/e2e_test.sh` consumes.
- **Upstream-shaped.** Written as a clean `scion/` feature dir + toggle so a
  PR to openshift-metal3/dev-scripts is possible; lives in a fork until
  then.

## 3. Topology

Two ASes in one ISD, both entirely on the hypervisor host, attached to the
dev-scripts baremetal bridge network (`EXTERNAL_SUBNET_V4`, default
`192.168.111.0/24`; host bridge IP `192.168.111.1`):

```
ISD 1
├── AS A  (cluster AS, default 1-ff00:0:110)      ── the AS the OpenShift
│     scion-cs-a   control service                   nodes join as endhosts
│     scion-br-a   border router
│     sigs: {}     managed by scion-registrar (node SIGs land here)
│
└── AS B  (remote AS, default 1-ff00:0:111)       ── simulates a remote site
      scion-cs-b   control service
      scion-br-b   border router
      scion-sig-b  SCION-IP gateway (tun on host, owns SCION_REMOTE_PREFIX)
```

AS A ↔ AS B are linked core-to-core via their BRs over loopback/bridge UDP.
All containers run `--net host` on the hypervisor; the two ASes get
non-overlapping port assignments in their topology files (SCION allows
arbitrary ports per service). Cluster VMs reach everything at
`192.168.111.1:<port>` across the libvirt bridge — clean L3, no NAT.

Support services (also on the host):

- `scion-discovery` (:8041): serves AS A's `topology.json` and TRCs in the
  netsec-ethz/bootstrapper HTTP layout (`/topology`, `/trcs`,
  `/trcs/<id>/blob`) — the agent's `bootstrap.mode=url` target. Implemented
  by the `serve-discovery.py` vendored from
  scion-k8s-operator/hack/dev-scion-topology (kept in sync manually; ~40
  lines).
- `scion-registrar` (:8642): scion-k8s-operator's AS-side registrar,
  patching AS A's `topology.json` `sigs` map and SIGHUP-reloading
  `scion-cs-a`. Bearer token generated into `$WORKING_DIR/scion/token`.

Remote ping target: a dummy interface on the host carries an address from
`SCION_REMOTE_PREFIX` (default `192.168.100.0/24`); `scion-sig-b` routes the
prefix through its tun, so cluster pods can ping it over SCION and the host
can originate inbound traffic toward pod/node IPs through SIG-B.

## 4. Files (in the dev-scripts fork)

```
scion/Dockerfile                     # single image, all binaries (see 4.1)
scion/serve-discovery.py             # vendored from scion-k8s-operator
scion/topology/as-a.topology.json.tpl
scion/topology/as-b.topology.json.tpl
scion/topology/testcrypto.topo.tpl   # input for scion-pki testcrypto
scion/configure_scion_as.sh
scion/cleanup_scion_as.sh
```

### 4.1 Dockerfile

Multi-stage, built at configure time as `localhost/scion-infra:$SCION_VERSION`:

- Stage 1 (golang builder): clone `scionproto/scion` at `$SCION_VERSION`
  (build-arg), `go build` `control`, `router`, `gateway`, `scion-pki`.
- Stage 2 (golang builder): `go install
  github.com/mkowalski/scion-k8s-operator/cmd/registrar@$SCION_OPERATOR_REF`
  (build-arg, default a pinned tag/commit).
- Final stage: `registry.access.redhat.com/ubi9/ubi-minimal` + python3
  (for serve-discovery.py) + all binaries. Exec-form entrypoints are chosen
  per container at `podman run` time (`--entrypoint`); scion binaries run as
  PID 1 so SIGHUP delivery via `podman kill -s HUP` works.

### 4.2 Topology templates

Rendered by `configure_scion_as.sh` with `envsubst`-style substitution:
bridge IP, per-AS ports, ISD-AS numbers. AS A's template sets
`dispatched_ports` to a range covering the node agents' SIG ports
(30056/30256/30856) and starts with an empty `sigs` map (registrar-managed).
AS B's template statically lists `sig-b`. Port plan (defaults; all on
192.168.111.1):

| Service | AS A | AS B |
|---|---|---|
| control service | 31000 | 32000 |
| BR internal | 31010 | 32010 |
| BR external (inter-AS link) | 31020 ↔ 32020 | |
| SIG ctrl/data/probe | (nodes: 30256/30056/30856) | 32056/32256/32856 |
| discovery | 8041 | — |
| registrar | 8642 | — |

Exact port keys and the testcrypto input schema MUST be verified against
scionproto `$SCION_VERSION` during implementation (topology JSON schema:
`private/topology/json/json.go`; testcrypto: `scion-pki/testcrypto`).

### 4.3 configure_scion_as.sh

Steps (idempotent, `--replace` semantics like the FRR script):

1. `podman build` the image (build-args `SCION_VERSION`,
   `SCION_OPERATOR_REF`).
2. Render topologies + testcrypto topo into `$WORKING_DIR/scion/`.
3. One-shot container: `scion-pki testcrypto` → TRCs/keys/certs under
   `$WORKING_DIR/scion/gen/`; copy per-AS `certs/`, `keys/`, `crypto/` into
   each AS's config dir per the layout the control service expects.
4. Create the dummy interface for `SCION_REMOTE_PREFIX`.
5. Start containers: `scion-cs-a`, `scion-br-a`, `scion-cs-b`, `scion-br-b`,
   `scion-sig-b` (privileged, `/dev/net/tun`), `scion-discovery`,
   `scion-registrar` (token from `$WORKING_DIR/scion/token`, generated if
   absent; `--reload-cmd "podman kill -s HUP scion-cs-a"`).
6. firewalld libvirt zone: the UDP ports above + TCP 8041, 8642.
7. Smoke checks: `curl :8041/topology` returns AS A's ISD-AS; an in-image
   `scion ping`? (optional — only if the `scion` CLI is added to the image;
   decide at implementation, not required).
8. Print the handoff block:

```
SCION topology ready:
  DISCOVERY_URL=http://192.168.111.1:8041
  REGISTRAR_URL=http://192.168.111.1:8642
  REGISTRAR_TOKEN=<from $WORKING_DIR/scion/token>
  REMOTE_ISD_AS=1-ff00:0:111
  REMOTE_PING_IP=192.168.100.1
```

### 4.4 cleanup_scion_as.sh

`podman rm -f` all seven containers, remove firewalld entries, delete the
dummy interface (and any leftover routes for `SCION_REMOTE_PREFIX`),
`rm -rf $WORKING_DIR/scion`. Tolerant of absence; hooked unconditionally
into `host_cleanup.sh` (mirrors `bgp/cleanup_bgp_tor.sh`).

## 5. Configuration variables

Defaults in `common.sh`, documentation in `config_example.sh`:

| Variable | Default | Meaning |
|---|---|---|
| `ENABLE_SCION_AS` | unset | Enable the feature |
| `SCION_VERSION` | `v0.15.0` | scionproto tag to build (must match the operator's pinned version) |
| `SCION_OPERATOR_REF` | pinned commit/tag | scion-k8s-operator ref for the registrar binary |
| `SCION_ISD_AS_A` | `1-ff00:0:110` | Cluster AS |
| `SCION_ISD_AS_B` | `1-ff00:0:111` | Remote AS |
| `SCION_REMOTE_PREFIX` | `192.168.100.0/24` | Prefix behind SIG-B (ping target) |
| `SCION_REGISTRAR_TOKEN` | generated | Registrar bearer token |

## 6. Testing

- Host-side smoke tests inside `configure_scion_as.sh` (step 7): discovery
  serves topology; registrar answers 401 without token / 200-family with;
  containers running; SIG-B tun exists.
- The real validation is the consumer flow: deploy scion-k8s-operator into
  the dev-scripts cluster, apply a ScionNetwork pointing at the handoff
  values, and run the operator repo's `test/e2e/e2e_test.sh` — this is the
  first live execution of the operator's dataplane (tracked in the operator
  repo's docs/known-gaps.md).
- shellcheck + `bash -n` for both scripts.

## 7. Risks

- `scion-pki testcrypto` input schema and the topology JSON port keys are
  version-sensitive; both must be verified against `$SCION_VERSION` sources
  during implementation (executor instruction, not an open design
  question).
- SIG-B manipulates host routing (tun + route for `SCION_REMOTE_PREFIX`) on
  the hypervisor; cleanup must be thorough or repeated runs will conflict.
- The core-link topology between AS A and AS B (core beaconing between two
  cores) is the simplest two-AS layout but must be expressed correctly in
  the templates (link type `CORE` both ways); a parent/child layout is the
  fallback if core-core beaconing misbehaves in a two-AS ISD.
- dev-scripts upstream acceptance is uncertain; the fork is the plan of
  record.

## 8. Non-goals

- Deploying the operator/agent into the cluster (handoff env vars only).
- SCIONLab or any external SCION connectivity.
- Multi-host topologies, IPv6, performance testing.
