# Known Gaps and Next Steps

Honest inventory of what has NOT been verified and what must happen before
this project is production-real. State as of the v0.1.0 merge to main
(commit 835d957). See also spec §9 (implementation deviations).

## Unverified — must be exercised before any real deployment

1. **Live dataplane run.** `test/integration/agent_test.sh` (netns pair +
   real SCION dev topology) has never been executed end-to-end. Tun creation,
   `NewStandaloneConnector`, SGRP prefix exchange, and route programming have
   only ever run in unit tests. Bring up `hack/dev-scion-topology` and run it
   (requires root + a scionproto checkout).
2. **Live e2e on OpenShift.** `test/e2e/e2e_test.sh` is syntax/shellcheck
   clean and cross-referenced against the code, but has never run against a
   cluster. Specifically untested: SCC/SELinux behavior of NET_ADMIN-only
   (non-privileged) tun access on RHCOS (`container_t` may block
   `/dev/net/tun`; privileged fallback is documented but unproven), operator
   RBAC under real non-admin credentials (envtest runs as cluster-admin),
   OLM bundle installation on-cluster, the `oc debug node` assertions.
3. **Registrar against a real control service.** SIGHUP topology reload is
   source-verified (scion v0.15.0 `private/topology/reload.go`), never run.
4. **OpenShift 5.x.** All platform research was validated against 4.x; every
   assumption (SCC, OVN-K SNAT path, `k8s.ovn.org/node-subnets` annotation,
   UWM) must be re-verified on 5.x.

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

1. Run gaps 1-3 above; fix fallout. This is the single highest-value next
   step.
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
   `as-registration.md`). If accepted, the registrar collapses to one
   standard mechanism.
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
8. Dependency watch: scionproto pinned at v0.15.0; `private/` package churn
   is expected on bumps — the two isolation files (`internal/agent/sig`,
   `internal/agent/daemonapi`) are the designated blast radius. The
   `grandcat/zeroconf` (mDNS) dependency is lightly maintained;
   `insomniacslk/dhcp` has no semver tags.
