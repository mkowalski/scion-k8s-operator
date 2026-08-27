# kind e2e — vanilla Kubernetes

`run.sh` proves the operator on plain Kubernetes (kind, default kindnetd
CNI): a two-node kind cluster plus a single-container two-AS SCION topology
(`Dockerfile.scion-as`) on the kind container network. It runs in CI on
every push/PR (`kind-e2e` job) and takes a few minutes.

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
- deleting the ScionNetwork deregisters the SIGs and removes the tun.

Masquerade: kindnetd masquerades pod→external traffic, which would both
break source preservation and mangle traffic onto the addressless tun. The
script exempts the remote SCION prefix on every node
(`KIND-MASQ-AGENT` RETURN rule). Real vanilla clusters need the equivalent
CNI-specific exclusion (ip-masq-agent `nonMasqueradeCIDRs`, Calico
`natOutgoing`, Cilium masquerade config) for the prefixes they accept from
remote ASes.

Local run (docker):

```sh
./test/e2e/kind/run.sh
```

Local run (podman):

```sh
CONTAINER_ENGINE=podman KIND_EXPERIMENTAL_PROVIDER=podman ./test/e2e/kind/run.sh
```

`KEEP=1` keeps the cluster and AS container for debugging.
