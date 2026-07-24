# scion-k8s-operator

Makes every node of an OpenShift cluster a first-class [SCION](https://scion.org)
endhost with transparent, bidirectional IP-over-SCION connectivity. A cluster
operator manages a per-node agent that bootstraps against the local SCION AS,
runs an embedded SCION daemon (path lookup, trust) and an embedded per-node
SCION-IP gateway (a `scion0` tun device with dynamic prefix exchange), so
non-SCION-aware workloads reach — and are reachable from — remote SCION
networks with zero changes.

## Architecture

```
scion-operator (Deployment, 1 replica, namespace scion-system)
  watches:  ScionNetwork (cluster-scoped CRD, singleton "cluster")
  manages:  scion-node-agent DaemonSet, SCC, ServiceAccount, RBAC,
            aggregated status conditions
        |                                    registrar controller
        v                                    (manual | http | anapaya)
scion-node-agent (DaemonSet, every node,          |
                  hostNetwork)                    | PUT /v1/sigs
  +-----------+  +---------------+  +----------+  v
  | bootstrap |  | daemon module |  | gateway  |  scion-registrar
  | topology  |->| paths, trust, |->| (SIG)    |  (AS-side service,
  | + TRCs    |  | gRPC :30255   |  | tun      |  patches topology.json
  +-----------+  | (opt-in)      |  | scion0,  |  sigs + SIGHUP-reloads
                 +---------------+  | SGRP     |  the control service)
                                    +----------+
                                          |
                                          v
                              SCION AS infrastructure
                        (control service, border router)
```

## Quick start

Prerequisites:

- An OpenShift cluster. OpenShift 5.x is the primary target; note the
  platform research behind this design (SCC, OVN-K shared-gateway SNAT,
  user-workload monitoring) was validated against OpenShift 4.x and is
  expected — but must be re-verified — to carry over to 5.x.
- A SCION AS you can attach to (open-source control service + border
  router, or Anapaya EDGE), with a way to register SIG endpoints — see
  [docs/as-registration.md](docs/as-registration.md).
- UDP reachability between cluster nodes and the remote SIGs on ports
  30056 (data), 30256 (SIG control), and 30856 (probe), both directions.

Install (kustomize path):

```sh
oc apply -k config/manifests
```

Image knobs: the operator image is `spec.template.spec.containers[].image`
in `config/manifests/operator.yaml`; the agent image is the `AGENT_IMAGE`
env var on the same container (overridable per-ScionNetwork via
`spec.agentImage`). Patch via a kustomize overlay or edit in place.

Then create the singleton ScionNetwork (name must be `cluster`):

```yaml
apiVersion: scion.mkowalski.github.io/v1alpha1
kind: ScionNetwork
metadata:
  name: cluster
spec:
  bootstrap:
    mode: url
    discoveryURL: http://scion-ds.example.org:8041
  acceptPolicy:
    isdASes:
      - 1-ff00:0:110
```

An OLM bundle is also available: `make bundle` regenerates `bundle/`, and
`operator-sdk run bundle` installs it. See
[docs/install.md](docs/install.md) for the full walkthrough, every
ScionNetwork field, pinned TRCs, and registrar credentials.

## Status and observability

- `oc get scionnetwork cluster` shows the discovered ISD-AS and ready node
  count; `status.conditions` carries `Available`, `Progressing`, and
  `Degraded`.
- Agents expose Prometheus metrics on port 9465; the operator on 8080.
  `config/manifests/monitoring.yaml` ships Services, ServiceMonitors, and
  a PrometheusRule with three alerts: `ScionNodeAgentDown`,
  `ScionNodeAgentAbsent` (user-workload-monitoring-safe fallback), and
  `ScionNetworkDegraded`.

## Development

Make targets: `build` (binaries into `bin/`), `test` (`go test ./...
-count=1`), `lint`, `manifests` (regenerate CRD from `api/`), `images`
(podman-build the three images, `IMG_REGISTRY`/`VERSION` knobs), `bundle`
(regenerate the OLM bundle), `bundle-check` (fail if `bundle/` is stale),
`envtest` (install setup-envtest for the controller suite).

A local multi-AS SCION dev topology plus a bootstrapping discovery server
lives in [hack/dev-scion-topology](hack/dev-scion-topology/README.md).
Integration tests: `test/integration/agent_test.sh` (two-netns end-to-end)
and `test/integration/discovery_test.sh`. OpenShift end-to-end:
`test/e2e/e2e_test.sh` (see [test/e2e/README.md](test/e2e/README.md)).

## License

Apache License 2.0 — see [LICENSE](LICENSE).
