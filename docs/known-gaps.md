# Known Gaps and Next Steps

Honest inventory of what has NOT been verified and what must happen before
this project is production-real. State as of the v0.1.0 merge to main
(commit 835d957), updated after the first live e2e run on OpenShift
(branch `live-e2e-fixes`, 2026-07-24), the SCION v0.15.1 dependency
upgrade, and source-preserving OVN-K e2e validation (2026-08-20).

## Retired by live e2e runs

Two full `test/e2e/e2e_test.sh` generations ran against a real five-node
OpenShift 5.0/RHCOS 10.2 cluster. The first used SCION v0.15.0 and exposed the
shared-gateway false positive. The final 2026-08-20 run used SCION v0.15.1,
OVN local-gateway routing, default `PodNetwork` route advertisements,
`underlayCIDRs`, and the hardened ten-container topology.

- **Live dataplane** (was gap 1): tun creation, `NewStandaloneConnector`,
  SGRP prefix exchange both directions, kernel route programming, and
  bidirectional ICMP pod↔remote through the SIG tunnel all verified live.
- **Live e2e on OpenShift**: the original suite completed end-to-end but its
  outbound assertion reached the target through the underlay. The hardened
  2026-08-20 run closed that false positive: an nftables guard blocked direct
  underlay delivery, pod ICMP and TCP traversed `scion0` and remote `sigb`, and
  the remote capture retained the pod source address (`10.128.2.49`).
  SCC/SELinux answer: NET_ADMIN-only nonroot does NOT work on RHCOS — the
  agent now runs privileged as root (TODO markers in `render.go`).
- **Registrar against a real control service** (was gap 3): operator→
  registrar HTTP sync, control-service topology reload, and remote-SIG
  gateway discovery of the registered node SIGs all observed working.
  Deregistration on ScionNetwork deletion now works via a finalizer.
  Deletion cannot wedge forever on a broken registrar: unimplemented stub
  backends (anapaya) release immediately, and other persistent Ensure
  failures are retried only until 10 minutes past the deletionTimestamp,
  after which the operator logs loudly and drops the finalizer anyway
  (stale SIG entries may remain on the AS side). Manual escape hatch:
  `kubectl patch scionnetwork cluster --type=merge -p
  '{"metadata":{"finalizers":null}}'`.
- **OpenShift 5.x** (was gap 4): SCC and `oc debug node` verified. The final
  design uses OVN-K local-gateway mode with default `PodNetwork` route
  advertisements; no operator-managed SNAT or OVN objects.
- **Image builds**: operator and agent images build and run
  (agent requires CGO for scionproto's go-sqlite3; glibc runtime base).
- **Remote-SIG churn panic**: scionproto/scion#4954 fixed the teardown race
  in `publishingRoutingTable`; the fix shipped in SCION v0.15.1, now pinned
  by the operator and dev topology.

Fixes made during the run (all on `live-e2e-fixes`): privileged agent
DaemonSet + SCC, CGO agent image, scionproto duplicate-metrics crash
(tolerant registration + daemon-API start ordering), scionproto logging
enabled (was silently discarded), traffic-policy Nets emptied (covering
set as Nets installed routes for nearly the whole IPv4 space on nodes),
per-interface IPv4 forwarding on the tun (RHCOS ships `ip_forward=0`),
registrar deregistration finalizer, e2e script fixes (node-IP
advertisement off, inbound ping source, printf bugs).

## New findings from the live runs — open design gaps

The OVN-K shared-gateway gap is resolved for clusters that opt into the two
documented platform prerequisites and configure all node-to-AS networks in
`acceptPolicy.underlayCIDRs`. The hardened suite proved that only the learned
destination selects `scion0`, ordinary and underlay destinations keep normal
routes, direct underlay delivery to the remote target is blocked, sources
survive decapsulation, TCP and bidirectional ICMP work, churn restores the
route, and undeploy removes tunnel and registration state.

1. **Node-IP advertisement is unsafe when the node IP shares the SCION
   underlay network.** Advertising node /32s made the remote SIG route the
   underlay itself into the tunnel (verified live: 272GB looped through the
   remote tun, blackholing probes and discovery). `advertisement.nodeIP`
   therefore now defaults to `false`; enable it only when node IPs are
   disjoint from the underlay. Overlap detection that would refuse or warn
   on an unsafe explicit `true` is still unimplemented.
2. **Registry/pull-secret assumptions in e2e.** The agent ServiceAccount is
   operator-owned and garbage-collected with the ScionNetwork; per-SA
   `imagePullSecrets` links are lost across undeploy/configure cycles.
   Fine with a properly authenticated cluster-wide registry, awkward otherwise.
3. **IPv6 dataplane remains unsupported.** Dual-stack OpenShift supplies IPv6
   cluster and service CIDRs; the operator now filters those from the agent's
   IPv4-only policy input so IPv4 SCION works on a dual-stack cluster. Native
   IPv6-over-SCION is still unimplemented.

## Not implemented (documented deviations, spec §9)

- Authenticated bootstrap (secretRef is pinned-TRCs only).
- `status.prefixes` advertised/learned counts (needs an agent→operator
  reporting channel).
- Dataplane MTU overrides.
- Agent bootstrap-state Prometheus metric (embedded scion metrics only).
- `status.isdAS` populated only for `url` bootstrap mode.
- Anapaya registrar backend is a stub (`ErrNotImplemented`); the integration
  surface is the Appliance Management API (OpenAPI client in
  `Anapaya/ansible-collections`).

## Follow-ups (rough priority order)

1. Decide the privileged-vs-scoped-SELinux story for the agent (temporary
   privileged fix shipped; a tailored SELinux policy + pre-chowned state
   dir could restore NET_ADMIN-only).
2. **CI.** No `.github/workflows` exists. Minimum: build, vet, unit tests,
   envtest (setup-envtest), `test/integration/discovery_test.sh`,
   `make bundle-check`, image builds. Aspirational: periodic e2e against a
   dev AS.
3. **API group.** `scion.mkowalski.github.io` is a placeholder tied to a
   personal GitHub Pages domain; revisit before any public release
   (CRD group changes are breaking — do it before anyone depends on
   v1alpha1).
4. **Upstream proposal.** File the TTL/heartbeat dynamic SIG
   self-registration design with scionproto/scion (draft paragraph in
   `as-registration.md`).
5. **Gateway readiness hook upstream.** `gateway.Run` exposes no "data plane
   up" signal; readiness is construction-based (decision D11). An upstream
   callback/channel would make readyz honest.
6. Node-churn hardening: Degraded currently flaps during rollouts (no 5m
   grace, TODO in `scionnetwork_controller.go`); pod-less nodes are counted
   in `status.nodes.total` but not named in `degraded`.
7. Operational polish: TLS for the registrar (plaintext bearer token today —
   trusted network assumed), metrics scrape auth on hostNetwork port 9465,
   dual-stack/IPv6 support (policy engine is deliberately IPv4-only),
   scale measurement of per-node SIG session fan-out (N nodes × M remote
   SIGs).
8. Dependency watch: scionproto pinned at v0.15.1; `private/` package churn
   is expected on bumps — the two isolation files (`internal/agent/sig`,
   `internal/agent/daemonapi`) are the designated blast radius. The
   `grandcat/zeroconf` (mDNS) dependency is lightly maintained;
   `insomniacslk/dhcp` has no semver tags.
