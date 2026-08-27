# kind e2e — vanilla Kubernetes

`run.sh` proves the operator on plain Kubernetes: a two-node kind cluster
plus a single-container two-AS SCION topology (`Dockerfile.scion-as`) on
the kind container network. Three CNI variants run in CI on every push/PR
(`kind-e2e` matrix, `CNI=kindnet|calico|cilium`), a few minutes each.

What it asserts:

- operator + agents deploy from the working tree (no OpenShift APIs: SCC
  skipped, platform condition `PlatformUnverified`, plaintext metrics),
- the forbidden list is derived generically — node `spec.podCIDRs` plus the
  `networking.k8s.io` ServiceCIDR API,
- a workload pod reaches a remote SCION target over `scion0` with its pod
  source preserved (the remote echo's access log must show the pod IP, and
  an nftables guard in the AS container drops anything that did not arrive
  through the remote SIG's tun),
- the remote site reaches the pod IP back through the tunnel,
- metrics are HTTPS with TokenReview auth even without service-ca: the
  operator-issued CA (ConfigMap `scion-node-agent-metrics-ca`) verifies the
  endpoint, an authorized ServiceAccount token gets 200, no token gets 401,
- deleting the ScionNetwork deregisters the SIGs and removes the tun.

Masquerade: both CNIs masquerade pod→external traffic, which would break
source preservation and mangle traffic onto the addressless tun. kindnet:
a `KIND-MASQ-AGENT` RETURN rule per node. Calico: a disabled IPPool over
the remote prefix with `disableBGPExport: true` (without it BIRD exports
the pool CIDR into the node mesh and the proto-bird route outranks the
SCION route). The Calico variant additionally proves BlockAffinity-based
pod-CIDR discovery — Calico ignores `node.spec.podCIDR`, so the agent must
advertise the IPAM blocks instead. The Cilium variant (helm, cluster-pool
IPAM, BPF ip-masq-agent) uses a pool disjoint from the node-allocator range
for the same reason: only CiliumNode-based advertisement makes it pass.

Local run (docker):

```sh
./test/e2e/kind/run.sh
```

Local run (podman, optionally with Calico):

```sh
CONTAINER_ENGINE=podman KIND_EXPERIMENTAL_PROVIDER=podman CNI=calico ./test/e2e/kind/run.sh
```

`KEEP=1` keeps the cluster and AS container for debugging.
