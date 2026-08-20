# Dev SCION topology

Bring up an upstream `scion.sh` topology for agent and discovery development.
This is the lightweight local option; the path-conclusive OpenShift topology
used by `test/e2e` lives on the `metal3-dev-scripts/scion-topology` branch and
adds registrar lifecycle, underlay exclusion, a remote SIG/TCP target, and
false-positive guards.

## Start the topology

```sh
git clone --depth 1 -b v0.15.1 https://github.com/scionproto/scion
cd scion
./scion.sh topology -c topology/tiny.topo
./scion.sh run
```

`tiny.topo` creates ISD 1 with ASes `1-ff00:0:110` (core), `1-ff00:0:111`,
and `1-ff00:0:112`, with all control services and routers on localhost.
Stop with `./scion.sh stop`.

This topology does not configure OVN-Kubernetes, `underlayCIDRs`, or the remote
`inet scion-e2e` guard. Use it for component development, not as evidence that
OpenShift pod traffic crossed SCION.

## Generated artifacts (layout)

`./scion.sh topology` writes everything under `gen/` in the scion checkout:

- `gen/ASff00_0_112/topology.json` — per-AS topology file (one
  `gen/AS<as-file-name>/` directory per AS; `:` in the AS number becomes `_`).
- `gen/trcs/ISD1-B1-S1.trc` — all TRCs, copied to a flat `trcs/` directory.
- `gen/ASff00_0_112/certs/` — the same TRCs, copied per AS.

Layout verified against the topology generator source at v0.15.1
(`tools/topology/cert.py`, `tools/topology/common.py:base_dir`,
`scion-pki/testcrypto/testcrypto.go` which populates `<out>/trcs/`).

## Serve a discovery endpoint for the agent

The agent bootstraps from a discovery server speaking the
netsec-ethz/bootstrapper HTTP layout:

- `GET /topology` → topology.json contents
- `GET /trcs` → JSON array of `{"id":{"isd":N,"base_number":N,"serial_number":N}}`
- `GET /trcs/isd{I}-b{B}-s{S}/blob` → raw TRC bytes

Serve AS `1-ff00:0:112`'s artifacts on port 8041:

```sh
./hack/dev-scion-topology/serve-discovery.py \
    /path/to/scion/gen/ASff00_0_112 \
    /path/to/scion/gen/trcs \
    8041
```

Then point a locally run agent at it (values depend on the generated AS):

```sh
SCION_DISCOVERY_URL=http://127.0.0.1:8041 \
SCION_LOCAL_PREFIXES=10.42.0.0/24 \
SCION_NODE_IP=127.0.0.1 \
./bin/agent
```

For the full OpenShift lifecycle use `test/e2e/README.md`; its
`UNDERLAY_CIDR` must contain every discovery, registrar, probe, and local-AS
transport address.

See `test/integration/agent_test.sh` for a full two-namespace end-to-end run
and `test/integration/discovery_test.sh` for a topology-free protocol check
of the discovery server itself.
