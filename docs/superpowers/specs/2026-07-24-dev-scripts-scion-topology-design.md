# dev-scripts SCION Topology Feature — As-Built Design

Date: 2026-07-24
Updated: 2026-08-20
Status: Implemented and live-validated
Implementation: `mkowalski/metal3-dev-scripts`, branch `scion-topology`, commit `b3a9137`

## 1. Purpose

`ENABLE_SCION_AS=true` creates a complete two-AS SCION v0.15.1 environment on
the dev-scripts hypervisor. OpenShift nodes join AS A as ordinary SCION
endhosts through scion-k8s-operator; AS B represents a remote site with a
SCION-IP Gateway and a path-conclusive IP target.

The feature is infrastructure-only. It does not install the operator. After
startup it prints the discovery, registrar, remote-AS, remote-IP, and TCP-port
values consumed by `test/e2e/e2e_test.sh`.

## 2. Topology

```text
OpenShift nodes (AS A endhosts)
        │ plain UDP underlay: 192.168.111.0/24
        ▼
┌──────────────────── hypervisor / AS A ────────────────────┐
│ scion-discovery :8041 ── topology.json + TRCs             │
│ scion-registrar :8642 ── manages node SIG entries         │
│ scion-cs-a ── control service                             │
│ scion-br-a ── border router                               │
│ scion-dispatcher-a ── legacy SCMP endpoint                │
└──────────────────────────┬─────────────────────────────────┘
                           │ SCION core link
┌──────────────────────────▼──── hypervisor / AS B ─────────┐
│ scion-cs-b ── control service                             │
│ scion-br-b ── border router                               │
│ scion-daemon-b + scion-sig-b (tun: sigb)                  │
│ scion-remote-echo :18080 bound to 192.168.100.1            │
│ nft inet scion-e2e: target accepted only from sigb         │
└────────────────────────────────────────────────────────────┘
```

Defaults:

- AS A: `1-ff00:0:110`
- AS B: `1-ff00:0:111`
- host/underlay address: `192.168.111.1`
- remote prefix: `192.168.100.0/24`; target `192.168.100.1`
- cluster pod prefixes: `10.128.0.0/14`

All ten long-running containers use host networking and one locally built
`localhost/scion-infra:v0.15.1` image:

1. `scion-cs-a`
2. `scion-br-a`
3. `scion-dispatcher-a`
4. `scion-cs-b`
5. `scion-br-b`
6. `scion-daemon-b`
7. `scion-sig-b`
8. `scion-remote-echo`
9. `scion-discovery`
10. `scion-registrar`

## 3. Data and control paths

The data test is deliberately path-conclusive:

1. AS B advertises only `SCION_REMOTE_PREFIX` to AS A.
2. Node agents learn that prefix through SGRP and install a route through
   `scion0`.
3. `inet scion-e2e` drops traffic to `REMOTE_PING_IP` unless it arrived on
   `sigb`; direct underlay delivery cannot produce a false positive.
4. The remote SIG accepts the configured pod prefixes and preserves their
   original sources.
5. ICMP captures on `sigb` and the HTTP endpoint on port 18080 validate both
   packet and stream traffic.

AS control-service replies need different routing. Once SIG-B learns pod
prefixes, the hypervisor main table routes those prefixes through `sigb`.
Discovery and registrar replies originate from `SCION_HOST_IP`, so rule/table
31050 sends that source back over the pre-existing dev-scripts underlay route.
Traffic sourced from `REMOTE_PING_IP` still uses `sigb`, preserving the inbound
SCION test.

On cluster nodes, the matching invariant is expressed by
`ScionNetwork.spec.acceptPolicy.underlayCIDRs`: node-to-AS transport networks
are subtracted from accepted SGRP prefixes and can never select `scion0`.

## 4. Generated state

`configure_scion_as.sh` writes under `$WORKING_DIR/scion`:

```text
as-a/                         rendered topology, service configs, crypto
as-b/                         rendered topology, daemon/SIG configs, crypto
gen/                          scion-pki testcrypto output
token                         registrar bearer token (0600)
testcrypto.topo               rendered crypto input
```

Topology templates live in `scion/topology/`. Trust material is regenerated
per configure run; named state volumes are removed before service restart so a
new base TRC cannot conflict with stale databases.

## 5. Ports

| Function | AS A | AS B |
|---|---:|---:|
| control service TCP/UDP | 31000 | 32000 |
| border-router internal UDP | 31002 | 32002 |
| inter-AS link UDP | 31020 | 32020 |
| node / remote SIG data UDP | 30056 | 32056 |
| node / remote SIG control UDP | 30256 | 32256 |
| node / remote SIG probe UDP | 30856 | 32856 |
| legacy dispatcher UDP | 30041 | — |
| discovery HTTP | 8041 | — |
| registrar HTTP | 8642 | — |
| remote TCP target | — | 18080 |

The libvirt firewalld zone exposes the SCION and HTTP ports. The trusted zone
accepts only configured cluster pod prefixes after decapsulation.

## 6. Configuration

| Variable | Default | Purpose |
|---|---|---|
| `ENABLE_SCION_AS` | unset | Enable topology creation |
| `SCION_VERSION` | `v0.15.1` | scionproto source tag |
| `SCION_OPERATOR_REF` | `main` | registrar source ref |
| `SCION_ISD_AS_A` | `1-ff00:0:110` | cluster-side AS |
| `SCION_ISD_AS_B` | `1-ff00:0:111` | simulated remote AS |
| `SCION_REMOTE_PREFIX` | `192.168.100.0/24` | remote SIG prefix |
| `SCION_CLUSTER_PREFIXES` | `10.128.0.0/14` | pod prefixes accepted by SIG-B |
| `SCION_REGISTRAR_TOKEN` | generated | fixed token override |

An IPv4 external subnet is required. The feature supports `IP_STACK=v4` and
`v4v6`; native IPv6-over-SCION is outside its scope.

## 7. Lifecycle

`configure_scion_as.sh`:

1. builds the pinned image;
2. renders topology and service configuration;
3. generates trust material;
4. installs policy routing, nftables, and firewalld state;
5. starts all containers;
6. checks discovery, registrar authentication, and container health;
7. prints the e2e handoff values.

`cleanup_scion_as.sh` removes all containers and state volumes, trusted-zone
sources, test-owned rule/table 31050, `inet scion-e2e`, `scion-remote`, `sigb`,
and `$WORKING_DIR/scion`. It deliberately leaves `podman.socket` enabled
because other host tooling may use it.

## 8. Validation

The final 2026-08-20 run used a five-node OpenShift 5.0/RHCOS 10.2 cluster with
OVN local-gateway routing and default `PodNetwork` route advertisements. It
proved:

- all five agents became Ready;
- learned remote routes selected `scion0`;
- direct underlay delivery to `192.168.100.1` was blocked;
- pod source `10.128.2.49` and host-selected source `10.128.2.2` survived
  decapsulation;
- TCP and bidirectional ICMP crossed SCION;
- agent replacement restored the route;
- deregistration, tun removal, route cleanup, and manifest teardown completed.

## 9. Boundaries and risks

- Testcrypto and `private/` topology schemas may change with scionproto; bump
  the operator and topology together.
- The registrar container mounts the Podman socket and therefore has
  root-equivalent host control. This is acceptable only for the development
  topology.
- Table/rule 31050 and `inet scion-e2e` are test-owned host state; cleanup must
  remain symmetrical.
- The feature remains on the fork branch until an upstream PR is explicitly
  approved.
