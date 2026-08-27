#!/usr/bin/env bash
# Entrypoint for the kind-e2e SCION AS container. Renders a two-AS core
# topology (A: the cluster's AS; B: a remote SIG site), mints test crypto,
# and runs every AS-side process in this single network namespace:
#   cs-a, br-a, dispatcher-a  — AS A control plane (cluster nodes join here)
#   cs-b, br-b, daemon-b, sig-b — AS B, the remote SCION site
#   discovery (8041), registrar (8642), remote HTTP echo (18080)
# The remote echo listens on REMOTE_PING_IP (a dummy interface); an nftables
# guard drops packets to it that did not arrive through the sigb tun, so a
# successful request is proof the path traversed SCION.
set -euo pipefail

ISD_AS_A=${ISD_AS_A:-1-ff00:0:110}
ISD_AS_B=${ISD_AS_B:-1-ff00:0:111}
REMOTE_PREFIX=${REMOTE_PREFIX:-192.168.100.0/24}
REMOTE_PING_IP=${REMOTE_PING_IP:-192.168.100.1}
REMOTE_TCP_PORT=${REMOTE_TCP_PORT:-18080}
CLUSTER_PREFIXES=${CLUSTER_PREFIXES:-10.244.0.0/16}
REGISTRAR_TOKEN=${REGISTRAR_TOKEN:?REGISTRAR_TOKEN must be set}

AS_IP=$(ip -4 -o addr show eth0 | awk '{split($4,a,"/"); print a[1]}')
IA_A_US=${ISD_AS_A//:/_}
IA_B_US=${ISD_AS_B//:/_}
W=/work
mkdir -p "$W"/{as-a,as-b}

# --- topology files -----------------------------------------------------------
cat > "$W/as-a/topology.json" <<EOF
{
  "isd_as": "$ISD_AS_A",
  "mtu": 1400,
  "dispatched_ports": "1024-65535",
  "attributes": ["core"],
  "control_service":   { "cs$IA_A_US-1": { "addr": "$AS_IP:31000" } },
  "discovery_service": { "cs$IA_A_US-1": { "addr": "$AS_IP:31000" } },
  "border_routers": {
    "br$IA_A_US-1": {
      "internal_addr": "$AS_IP:31002",
      "interfaces": {
        "1": {
          "underlay": { "local": "$AS_IP:31020", "remote": "$AS_IP:32020" },
          "isd_as": "$ISD_AS_B", "link_to": "CORE", "mtu": 1400
        }
      }
    }
  },
  "sigs": {}
}
EOF
cat > "$W/as-b/topology.json" <<EOF
{
  "isd_as": "$ISD_AS_B",
  "mtu": 1400,
  "dispatched_ports": "1024-65535",
  "attributes": ["core"],
  "control_service":   { "cs$IA_B_US-1": { "addr": "$AS_IP:32000" } },
  "discovery_service": { "cs$IA_B_US-1": { "addr": "$AS_IP:32000" } },
  "border_routers": {
    "br$IA_B_US-1": {
      "internal_addr": "$AS_IP:32002",
      "interfaces": {
        "1": {
          "underlay": { "local": "$AS_IP:32020", "remote": "$AS_IP:31020" },
          "isd_as": "$ISD_AS_A", "link_to": "CORE", "mtu": 1400
        }
      }
    }
  },
  "sigs": {
    "sig$IA_B_US-1": {
      "ctrl_addr": "$AS_IP:32256",
      "data_addr": "$AS_IP:32056",
      "probe_addr": "$AS_IP:32856"
    }
  }
}
EOF

# --- trust material -----------------------------------------------------------
cat > "$W/testcrypto.topo" <<EOF
ASes:
  "$ISD_AS_A": { core: true, voting: true, authoritative: true, issuing: true }
  "$ISD_AS_B": { core: true, voting: true, authoritative: true, issuing: true }
EOF
scion-pki testcrypto -t "$W/testcrypto.topo" -o "$W/gen" --as-validity 30d
for as in a b; do
    ia_var="IA_${as^^}_US"; ia=${!ia_var}
    src="$W/gen/AS${ia#*-}"
    dst="$W/as-$as"
    cp -r "$src/crypto" "$dst/"
    mkdir -p "$dst/certs" "$dst/keys"
    cp "$W"/gen/trcs/*.trc "$dst/certs/"
    for k in master0.key master1.key; do
        head -c16 /dev/urandom | base64 > "$dst/keys/$k"
    done
done

# --- service configs ----------------------------------------------------------
for as in a b; do
    ia_var="IA_${as^^}_US"; ia=${!ia_var}
    mkdir -p "/var/lib/scion-$as"
    cat > "$W/as-$as/cs.toml" <<EOF
[general]
id = "cs$ia-1"
config_dir = "$W/as-$as"
[trust_db]
connection = "/var/lib/scion-$as/cs.trust.db"
[beacon_db]
connection = "/var/lib/scion-$as/cs.beacon.db"
[path_db]
connection = "/var/lib/scion-$as/cs.path.db"
EOF
    cat > "$W/as-$as/br.toml" <<EOF
[general]
id = "br$ia-1"
config_dir = "$W/as-$as"
EOF
done
cat > "$W/as-a/dispatcher.toml" <<EOF
[dispatcher]
id = "dispatcher$IA_A_US"
underlay_addr = "$AS_IP"
local_udp_forwarding = true
EOF
cat > "$W/as-b/daemon.toml" <<EOF
[general]
id = "sd$IA_B_US"
config_dir = "$W/as-b"
[sd]
address = "127.0.0.1:32255"
[path_db]
connection = "/var/lib/scion-b/sd.path.db"
[trust_db]
connection = "/var/lib/scion-b/sd.trust.db"
EOF
cat > "$W/as-b/sig.toml" <<EOF
[gateway]
id = "sig$IA_B_US-1"
traffic_policy_file = "$W/as-b/sig-traffic.json"
ip_routing_policy_file = "$W/as-b/sig-routing.policy"
ctrl_addr = "$AS_IP:32256"
data_addr = "$AS_IP:32056"
probe_addr = "$AS_IP:32856"
[sciond_connection]
address = "127.0.0.1:32255"
[tunnel]
name = "sigb"
EOF
# shellcheck disable=SC2086  # intentional word splitting of the prefix list
nets_json=$(printf '"%s",' ${CLUSTER_PREFIXES//,/ }); nets_json="[${nets_json%,}]"
cat > "$W/as-b/sig-traffic.json" <<EOF
{ "ConfigVersion": 1, "ASes": { "$ISD_AS_A": { "Nets": $nets_json } } }
EOF
{
    echo "advertise $ISD_AS_B $ISD_AS_A $REMOTE_PREFIX"
    echo "accept    $ISD_AS_A $ISD_AS_B $CLUSTER_PREFIXES"
} > "$W/as-b/sig-routing.policy"

# --- remote target + path-conclusiveness guard --------------------------------
ip link add scion-remote type dummy
ip addr add "$REMOTE_PING_IP/${REMOTE_PREFIX#*/}" dev scion-remote
ip link set scion-remote up
nft add table inet scion-e2e
nft add chain inet scion-e2e input '{ type filter hook input priority -5; policy accept; }'
nft add rule inet scion-e2e input ip daddr "$REMOTE_PING_IP" iifname != "sigb" counter drop

# --- run everything -----------------------------------------------------------
# The background job is the component binary itself (log prefixing runs in a
# process substitution outside the job), so `wait -n` only reaps real
# component deaths — a SIGHUP-driven topology reload of scion-control must
# not take the wrapper pipeline down with it.
run() { local name="$1"; shift; "$@" > >(sed -u "s/^/[$name] /") 2>&1 & }
run cs-a       scion-control --config "$W/as-a/cs.toml"
run br-a       scion-router --config "$W/as-a/br.toml"
run disp-a     scion-dispatcher --config "$W/as-a/dispatcher.toml"
run cs-b       scion-control --config "$W/as-b/cs.toml"
run br-b       scion-router --config "$W/as-b/br.toml"
run daemon-b   scion-daemon --config "$W/as-b/daemon.toml"
run sig-b      scion-ip-gateway --config "$W/as-b/sig.toml"
run discovery  python3 /usr/local/bin/serve-discovery.py "$W/as-a" "$W/as-a/certs" 8041
run echo       python3 -m http.server "$REMOTE_TCP_PORT" --bind "$REMOTE_PING_IP"
REGISTRAR_TOKEN="$REGISTRAR_TOKEN" run registrar scion-registrar \
    -topology "$W/as-a/topology.json" -listen "$AS_IP:8642" \
    -reload-cmd "pkill -HUP -x scion-control"

echo "[entrypoint] SCION AS ready on $AS_IP (discovery :8041, registrar :8642, echo $REMOTE_PING_IP:$REMOTE_TCP_PORT)"
# Exit (and thereby fail the container) as soon as any component dies.
wait -n
echo "[entrypoint] a component exited; failing" >&2
exit 1
