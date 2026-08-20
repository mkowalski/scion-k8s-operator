# End-to-end tests on OpenShift

`e2e_test.sh` exercises the full operator lifecycle against a real OpenShift
cluster: install, configure, agent rollout, dataplane connectivity, SIG
registration, pod churn, and teardown. Each phase prints `OK <phase>` on
success; the first failure prints `FAILED phase: <phase>` and exits non-zero.

## Prerequisites

- An OpenShift cluster; `oc` logged in (or `KUBECONFIG` set) with
  cluster-admin.
- A dev SCION AS reachable **from the cluster nodes** (not just from your
  workstation). See `hack/dev-scion-topology/README.md` for a two-AS dev
  topology and the bundled discovery server (`serve-discovery.py`).
- The registrar service (`cmd/registrar`) running next to that AS's control
  service, with `REGISTRAR_TOKEN` set on the service side.
- Operator and agent images pushed to a registry the cluster can pull, and
  the manifests pointing at them. Two image knobs:
  - operator image: `spec.template.spec.containers[].image` in
    `config/manifests/operator.yaml` (patch via kustomize `images:` or edit).
  - agent image: the `AGENT_IMAGE` env var on the operator container in the
    same file (can also be overridden per-ScionNetwork via
    `spec.agentImage`).
- UDP ports 30056 (data), 30256 (SIG control), and 30856 (SIG probe) open
  between the cluster nodes and the AS host, in both directions.
- OVN-Kubernetes configured for source-preserving host routing:
  `routingViaHost: true`, `routeAdvertisements: Enabled`, and an accepted
  default-network `RouteAdvertisements` resource advertising `PodNetwork`.
  The suite checks these settings before creating the `ScionNetwork`.

## Environment variables

| Variable          | Required by                                  | Description |
|-------------------|----------------------------------------------|-------------|
| `DISCOVERY_URL`   | `configure`                                  | Bootstrap discovery server URL, e.g. `http://as-host:8041` |
| `REMOTE_ISD_AS`   | `configure`                                  | Remote ISD-AS to accept prefixes from, e.g. `1-ff00:0:111` |
| `REGISTRAR_URL`   | `configure`, `assert_registration`, `churn`, `undeploy` | Registrar base URL, e.g. `http://as-host:8642` |
| `REGISTRAR_TOKEN` | same as `REGISTRAR_URL`                      | Bearer token for the registrar |
| `REMOTE_PING_IP`  | `assert_dataplane`, `churn`, `undeploy`      | An IP behind the remote SIG |
| `UNDERLAY_CIDR`   | optional (`configure`)                       | Cluster-to-AS transport CIDR excluded from learned SCION routes (default `192.168.111.0/24`) |
| `REMOTE_SSH`      | `assert_dataplane`                           | SSH destination on the remote side; used to verify the underlay guard, capture the inner source addresses, and initiate inbound traffic. Set to `local` when running the suite on the AS host itself |
| `REMOTE_TCP_PORT` | optional                                     | TCP test endpoint behind the remote SIG (default `18080`) |
| `NON_SCION_IP`    | optional                                     | Control destination that must keep its normal route (default `1.1.1.1`) |
| `TEST_IMAGE`      | optional                                     | Test pod image (default `registry.fedoraproject.org/fedora-toolbox:latest` — chosen because its iputils `ping` works unprivileged via ICMP datagram sockets; ubi-minimal/ubi/fedora base images ship no `ping`, and busybox `ping` needs raw sockets. The image must also provide `curl`) |
| `TUN_NAME`        | optional                                     | tun device name (default `scion0`) |
| `PHASES`          | optional                                     | Space-separated subset of phases (default: all) |
| `KEEP_OPERATOR`   | optional (`undeploy`)                        | Set to `1` to leave the operator installed after teardown |

## Running

Full run:

```sh
DISCOVERY_URL=http://192.0.2.10:8041 \
REMOTE_ISD_AS=1-ff00:0:111 \
REGISTRAR_URL=http://192.0.2.10:8642 \
REGISTRAR_TOKEN=devtoken \
REMOTE_PING_IP=203.0.113.5 \
REMOTE_SSH=user@192.0.2.10 \
./test/e2e/e2e_test.sh
```

Single phase (against an already-deployed setup):

```sh
REGISTRAR_URL=http://192.0.2.10:8642 REGISTRAR_TOKEN=devtoken \
PHASES=assert_registration ./test/e2e/e2e_test.sh
```

Phases in order: `deploy configure assert_agents assert_dataplane
assert_registration churn undeploy`.

## Troubleshooting

- **Agent pods CrashLoop**: almost always bootstrap. Verify the discovery
  URL is reachable *from a node*: `oc debug node/<node> -- chroot /host
  curl -v $DISCOVERY_URL/topology`. Firewalls between nodes and the AS host
  are the usual culprit.
- **ScionNetwork Degraded**: `oc get scionnetwork cluster -o
  yaml` — check `status.conditions[?(@.type=="Degraded")].message` and
  `status.nodes.degraded` for which nodes are unhealthy, then read that
  node's agent pod logs (`oc -n scion-system logs ds/scion-node-agent`).
- **Registrar 401**: token mismatch between the Secret
  `scion-system/scion-registrar-token` (key `token`) and the
  `REGISTRAR_TOKEN` env of the registrar service. The operator surfaces this
  in `status.registrar.lastError`.
- **Outbound traffic fails but agents are Ready**: confirm `routingViaHost`
  and route advertisements with `oc get network.operator cluster -o yaml`
  and `oc get routeadvertisements.k8s.ovn.org -o yaml`; then check that the
  remote AS accepted SIG registration and UDP 30056/30256/30856 is open
  node↔AS in both directions. The dataplane phase rejects a direct-underlay
  false positive and captures the pod and node source addresses on `sigb`.
