# Known Gaps and Next Steps

Honest inventory of what has NOT been verified and what must happen before
this project is production-real. State as of the v0.1.0 merge to main
(commit 835d957), updated after the first live e2e run on OpenShift
(branch `live-e2e-fixes`, 2026-07-24). See also spec §9 (implementation
deviations).

## Retired by the live e2e run (2026-07-24)

The full `test/e2e/e2e_test.sh` suite (deploy, configure, assert_agents,
assert_dataplane, assert_registration, churn, undeploy) passed against a
real 5-node OpenShift 5.x nightly (RHCOS 10.2, OVN-Kubernetes, baremetal
dev-scripts) with a live SCION dev topology (scionproto v0.15.0 control
service, border routers, discovery server, registrar, remote SIG). What
this retired:

- **Live dataplane** (was gap 1): tun creation, `NewStandaloneConnector`,
  SGRP prefix exchange both directions, kernel route programming, and
  bidirectional ICMP pod↔remote through the SIG tunnel all verified live.
- **Live e2e on OpenShift** (was gap 2): the suite completes end-to-end,
  with one asterisk: the outbound dataplane assertion is a false positive
  for the SCION path in the test topology (pod egress reached the remote
  SIG over the underlay, not the tunnel — see new finding 1). All other
  assertions are genuine.
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
- **OpenShift 5.x** (was gap 4): SCC and `oc debug node` verified; the
  OVN-K SNAT-path assumption was DISPROVEN — see below.
- **Image builds**: operator and agent images build and run
  (agent requires CGO for scionproto's go-sqlite3; glibc runtime base).

Fixes made during the run (all on `live-e2e-fixes`): privileged agent
DaemonSet + SCC, CGO agent image, scionproto duplicate-metrics crash
(tolerant registration + daemon-API start ordering), scionproto logging
enabled (was silently discarded), traffic-policy Nets emptied (covering
set as Nets installed routes for nearly the whole IPv4 space on nodes),
per-interface IPv4 forwarding on the tun (RHCOS ships `ip_forward=0`),
registrar deregistration finalizer, e2e script fixes (node-IP
advertisement off, inbound ping source, printf bugs).

## New findings from the live run — open design gaps

1. **OVN-K shared gateway bypasses host routes for pod egress.** The design
   assumed pod egress reaches the host routing table (research.md §OVN);
   with the OpenShift default `routingViaHost: false` pod egress goes
   pod → OVN gateway router → br-ex with SNAT and never consults host
   routes. Verified live by packet capture. Consequences:
   - The e2e "outbound" ping succeeds only because the remote SIG host is
     also reachable over the underlay in the test topology (false
     positive for the SCION path). In a real deployment, pod-originated
     traffic to SCION prefixes will NOT enter the tunnel.
   - Inbound remote→pod DOES traverse SCION end-to-end and replies return
     correctly (mp0 conntrack keeps the reply path symmetric) — verified.
   - Options to close: require/document `routingViaHost: true` (local
     gateway mode), or integrate with OVN-K admin policy based external
     routes, or an eBPF/TC steering shim. Needs a design decision.
2. **Node-IP advertisement is unsafe when the node IP shares the SCION
   underlay network.** Advertising node /32s made the remote SIG route the
   underlay itself into the tunnel (verified live: 272GB looped through the
   remote tun, blackholing probes and discovery). The operator should
   refuse or warn when advertised node IPs overlap the underlay path;
   `advertisement.nodeIP: false` is the safe default in single-NIC
   topologies.
3. **scionproto v0.15.0 remote-SIG panic under session churn.** The dev
   topology's remote SIG (upstream scionproto gateway binary) crashed with
   a nil-pointer panic in `publishingRoutingTable.ClearSession`
   (`gateway/control/publishingroutingtable.go:165`) while agent pods were
   restarting. Upstream defect; affects any SIG peer under churn.
4. **Registry/pull-secret assumptions in e2e.** The agent ServiceAccount is
   operator-owned and garbage-collected with the ScionNetwork; per-SA
   `imagePullSecrets` links are lost across undeploy/configure cycles.
   Fine with a properly authed cluster-wide registry, awkward otherwise.
5. **Test weaknesses noticed:** `wait_agents_ready` can pass against stale
   DaemonSet status immediately after pod deletion (churn re-check raced);
   the outbound dataplane assertion cannot distinguish SCION-path from
   underlay-path delivery (see finding 1).

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

1. Resolve the OVN-K shared-gateway egress gap (new finding 1) — the
   consumer outbound flow is not real until this is closed.
2. Decide the privileged-vs-scoped-SELinux story for the agent (temporary
   privileged fix shipped; a tailored SELinux policy + pre-chowned state
   dir could restore NET_ADMIN-only).
3. **CI.** No `.github/workflows` exists. Minimum: build, vet, unit tests,
   envtest (setup-envtest), `test/integration/discovery_test.sh`,
   `make bundle-check`, image builds. Aspirational: periodic e2e against a
   dev AS.
4. **API group.** `scion.mkowalski.github.io` is a placeholder tied to a
   personal GitHub Pages domain; revisit before any public release
   (CRD group changes are breaking — do it before anyone depends on
   v1alpha1).
5. **Upstream proposal.** File the TTL/heartbeat dynamic SIG
   self-registration design with scionproto/scion (draft paragraph in
   `as-registration.md`). Also report the `ClearSession` panic (new
   finding 3).
6. **Gateway readiness hook upstream.** `gateway.Run` exposes no "data plane
   up" signal; readiness is construction-based (decision D11). An upstream
   callback/channel would make readyz honest.
7. Node-churn hardening: Degraded currently flaps during rollouts (no 5m
   grace, TODO in `scionnetwork_controller.go`); pod-less nodes are counted
   in `status.nodes.total` but not named in `degraded`.
8. Operational polish: TLS for the registrar (plaintext bearer token today —
   trusted network assumed), metrics scrape auth on hostNetwork port 9465,
   dual-stack/IPv6 support (policy engine is deliberately IPv4-only),
   scale measurement of per-node SIG session fan-out (N nodes × M remote
   SIGs).
9. Dependency watch: scionproto pinned at v0.15.0; `private/` package churn
   is expected on bumps — the two isolation files (`internal/agent/sig`,
   `internal/agent/daemonapi`) are the designated blast radius. The
   `grandcat/zeroconf` (mDNS) dependency is lightly maintained;
   `insomniacslk/dhcp` has no semver tags.
