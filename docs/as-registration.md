# AS-side SIG registration

For inbound traffic to work, the AS must know about each node's embedded
SCION-IP gateway (SIG): remote SIGs discover peers through the `sigs` map
in the AS `topology.json` served by the control service. This document
covers the three registration backends (`spec.registrar.backend`), the
AS-side registrar service, known limitations, and planned upstream work.

Every node contributes one SIG entry: control address `<nodeIP>:30256`,
data address `<nodeIP>:30056` (probe on 30856). The operator computes the
desired set from the nodes matching `spec.nodeSelector` and publishes it in
`status.registrar.desiredSIGs` regardless of backend.

## Manual backend (default)

The operator only publishes the desired set; the AS operator applies it.

1. Read the desired SIGs:

   ```sh
   oc get scionnetwork cluster -o jsonpath='{.status.registrar.desiredSIGs}'
   ```

   Each entry has the form `name=ctrlAddr,dataAddr`, e.g.
   `worker-0=10.0.0.5:30256,10.0.0.5:30056`.

2. Edit the AS `topology.json` and add one entry per node to the `sigs`
   map (entry names are your choice; no prefix convention applies in
   manual mode):

   ```json
   "sigs": {
     "worker-0": {
       "ctrl_addr": "10.0.0.5:30256",
       "data_addr": "10.0.0.5:30056"
     }
   }
   ```

   Field names match scion v0.15.0 `private/topology/json` GatewayInfo
   (`ctrl_addr`, `data_addr`; `probe_addr` and `allow_interfaces` are
   optional and not managed by the operator).

3. Reload the control service. SCION v0.15.0 reloads its topology on
   SIGHUP with no control-plane downtime:

   ```sh
   systemctl kill -s HUP scion-control
   ```

Repeat when nodes are added or removed (watch `desiredSIGs`).

## HTTP backend: the registrar service

`cmd/registrar` is a small AS-side service that automates the manual steps:
it patches the operator-managed subset of `sigs` in `topology.json`
atomically and runs a reload command. It exposes:

- `PUT /v1/sigs` — replace the full operator-managed SIG set; `204 No
  Content` means the topology was patched and the control service
  reloaded.
- `GET /v1/sigs` — return the currently managed set (derived from the
  file, so accurate across restarts).

All requests require `Authorization: Bearer <token>`; an unset
`REGISTRAR_TOKEN` makes the service refuse to start (fail-closed). Managed
entries are written with a name prefix (default `k8s-`); entries without
the prefix are never touched, so operator-managed and hand-managed SIGs
coexist.

Flags: `--topology` (default `/etc/scion/topology.json`), `--prefix`
(default `k8s-`), `--listen` (default `:8642`), `--reload-cmd` (default
`systemctl kill -s HUP scion-control`; split on spaces, no shell quoting).

The registrar typically runs via systemd on the AS host (it needs access
to the host's systemd for the reload; a container image exists but has the
same constraint):

```ini
# /etc/systemd/system/scion-registrar.service
[Unit]
Description=SCION k8s registrar
After=network-online.target scion-control.service

[Service]
Environment=REGISTRAR_TOKEN=<token>
ExecStart=/usr/local/bin/registrar \
    --topology /etc/scion/topology.json \
    --listen :8642
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Security caveat: the bearer token travels in plaintext HTTP. Deploy the
registrar on a trusted or tunneled network (e.g. WireGuard) or behind a
TLS-terminating reverse proxy.

Cluster-side configuration:

```yaml
spec:
  registrar:
    backend: http
    endpoint: http://as-host:8642
    credentialsSecretRef:
      name: scion-registrar-token   # key "token" in scion-system
```

On every reconcile the operator PUTs the full desired set; sync results
appear in `status.registrar` (`registeredNodes`, `lastSyncTime`,
`lastError`), and a failing sync marks the ScionNetwork `Degraded`
(reason `RegistrarSyncFailed`).

## Anapaya backend (stub)

`backend: anapaya` is declared in the API but not implemented: `Ensure`
returns `ErrNotImplemented`, which surfaces in `status.registrar.lastError`
and `Degraded`. The intended integration surface is the OpenAPI client
models shipped in Anapaya/ansible-collections
(`plugins/module_utils/appliance_api_client`), PATCHing the appliance
gateway configuration with the desired SIG set. Until then, use
`backend: http` with the registrar service, or `backend: manual` and
configure the appliance from `desiredSIGs`.

## Known limitation: stale entries after unclean cluster removal

If a cluster is deleted without removing the ScionNetwork first (or the
operator never gets to run a final sync), its `k8s-`-prefixed entries
remain in the AS topology. Mitigations:

- The registrar reconciles the full managed set on every PUT: the next
  sync from any operator using the same registrar (and prefix) replaces
  the whole managed set, clearing entries of nodes that no longer exist
  in that cluster.
- Manual cleanup: `PUT /v1/sigs` with an empty JSON object (`{}`) removes
  all managed entries and reloads the control service —

  ```sh
  curl -X PUT -H "Authorization: Bearer $REGISTRAR_TOKEN" \
      -H "Content-Type: application/json" -d '{}' \
      http://as-host:8642/v1/sigs
  ```

  — or edit `topology.json` by hand, deleting the `k8s-`-prefixed `sigs`
  entries, and SIGHUP the control service.

## Upstream follow-up: dynamic SIG self-registration

The registrar exists only because SCION has no dynamic gateway discovery:
the `sigs` map is static file-based configuration. We plan to propose to
scionproto a TTL/heartbeat-based self-registration mechanism, which would
make the entire registrar path obsolete. Draft summary intended to seed
the upstream issue (not yet filed):

> SIG endpoint discovery is currently static (`sigs` in topology.json),
> which makes dynamic environments — Kubernetes nodes, autoscaling groups,
> ephemeral test setups — require out-of-band tooling to patch and reload
> the control service. Proposal: allow gateways to self-register their
> ctrl/data endpoints with the control service over an authenticated API,
> with a TTL refreshed by heartbeat, so entries expire when a gateway
> disappears uncleanly. Static `sigs` entries remain supported and
> authoritative; dynamic registrations complement them.
