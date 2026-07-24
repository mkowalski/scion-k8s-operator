#!/usr/bin/env bash
# agent_test.sh: end-to-end integration test for the scion-node-agent.
#
# REQUIREMENTS (fails fast if missing):
#   - root (netns/veth/tun manipulation): run with sudo.
#   - A local dev SCION topology up and running (see
#     hack/dev-scion-topology/README.md): scion checkout at $SCION_DIR with
#     `./scion.sh topology -c topology/tiny.topo && ./scion.sh run` done.
#   - Two discovery servers (serve-discovery.py) serving two different ASes,
#     reachable at $DISCOVERY_URL_A and $DISCOVERY_URL_B (defaults below
#     assume ports 8041/8042 on the host).
#   - The agent binary built: `go build -o bin/agent ./cmd/agent`.
#
# What it does:
#   - Creates network namespaces sig-a and sig-b, each connected via a veth
#     pair to the host (where the scion topology's services listen).
#   - Runs one agent per namespace with SCION_LOCAL_PREFIXES/SCION_NODE_IP
#     (bypassing Kubernetes) and SCION_DISCOVERY_URL.
#   - Asserts: scion0 tun exists in each ns, a route to the peer's prefix
#     appears, and ping across the SCION-IP tunnel succeeds.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"

AGENT_BIN="${AGENT_BIN:-$repo/bin/agent}"
DISCOVERY_URL_A="${DISCOVERY_URL_A:-http://10.99.0.1:8041}"   # AS for sig-a (e.g. 1-ff00:0:111)
DISCOVERY_URL_B="${DISCOVERY_URL_B:-http://10.99.1.1:8042}"   # AS for sig-b (e.g. 1-ff00:0:112)
TIMEOUT="${TIMEOUT:-60}"

# --- prerequisites -----------------------------------------------------------
[[ "$(id -u)" -eq 0 ]] || { echo "FATAL: must run as root (sudo $0)" >&2; exit 1; }
[[ -x "$AGENT_BIN" ]] || { echo "FATAL: agent binary not found at $AGENT_BIN; build with 'go build -o bin/agent ./cmd/agent'" >&2; exit 1; }
for cmd in ip curl ping; do
    command -v "$cmd" >/dev/null || { echo "FATAL: '$cmd' not found" >&2; exit 1; }
done
for url in "$DISCOVERY_URL_A" "$DISCOVERY_URL_B"; do
    curl -fsS -m 5 -o /dev/null "$url/topology" || {
        echo "FATAL: discovery server not reachable at $url — is the dev topology + serve-discovery.py running? See hack/dev-scion-topology/README.md" >&2
        exit 1
    }
done

# --- topology: host bridge + two namespaces ---------------------------------
# sig-a: 10.99.0.2 (host side 10.99.0.1), advertises 10.42.0.0/24
# sig-b: 10.99.1.2 (host side 10.99.1.1), advertises 10.42.1.0/24
PREFIX_A="10.42.0.0/24"; IP_A="10.99.0.2"; HOST_A="10.99.0.1"
PREFIX_B="10.42.1.0/24"; IP_B="10.99.1.2"; HOST_B="10.99.1.1"
LOOP_A="10.42.0.1"; LOOP_B="10.42.1.1"   # addrs inside the advertised prefixes

pids=()
cleanup() {
    for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null || true; done
    wait 2>/dev/null || true
    ip netns del sig-a 2>/dev/null || true
    ip netns del sig-b 2>/dev/null || true
    ip link del veth-a 2>/dev/null || true
    ip link del veth-b 2>/dev/null || true
    rm -rf /tmp/scion-agent-test-a /tmp/scion-agent-test-b
}
trap cleanup EXIT

for ns in sig-a sig-b; do
    ip netns del "$ns" 2>/dev/null || true
    ip netns add "$ns"
    ip netns exec "$ns" ip link set lo up
done

setup_link() { # ns hostif nsip hostip
    local ns="$1" hostif="$2" nsip="$3" hostip="$4"
    ip link del "$hostif" 2>/dev/null || true
    ip link add "$hostif" type veth peer name eth0 netns "$ns"
    ip addr add "$hostip/30" dev "$hostif"
    ip link set "$hostif" up
    ip netns exec "$ns" ip addr add "$nsip/30" dev eth0
    ip netns exec "$ns" ip link set eth0 up
    ip netns exec "$ns" ip route add default via "$hostip"
}
setup_link sig-a veth-a "$IP_A" "$HOST_A"
setup_link sig-b veth-b "$IP_B" "$HOST_B"

# Loopback addresses within each advertised prefix, to ping across the tunnel.
ip netns exec sig-a ip addr add "$LOOP_A/32" dev lo
ip netns exec sig-b ip addr add "$LOOP_B/32" dev lo

# --- run the agents ----------------------------------------------------------
run_agent() { # ns prefix nodeip discovery statedir metricsport
    local ns="$1" prefix="$2" nodeip="$3" discovery="$4" statedir="$5" mport="$6"
    mkdir -p "$statedir"
    ip netns exec "$ns" env \
        SCION_BOOTSTRAP_MODE=url \
        SCION_DISCOVERY_URL="$discovery" \
        SCION_LOCAL_PREFIXES="$prefix" \
        SCION_NODE_IP="$nodeip" \
        SCION_ADVERTISE_NODE_IP=false \
        SCION_STATE_DIR="$statedir" \
        SCION_METRICS_ADDR=":$mport" \
        "$AGENT_BIN" >"$statedir/agent.log" 2>&1 &
    pids+=($!)
}
run_agent sig-a "$PREFIX_A" "$IP_A" "$DISCOVERY_URL_A" /tmp/scion-agent-test-a 9465
run_agent sig-b "$PREFIX_B" "$IP_B" "$DISCOVERY_URL_B" /tmp/scion-agent-test-b 9466

wait_for() { # desc timeout cmd...
    local desc="$1" t="$2"; shift 2
    local deadline=$((SECONDS + t))
    until "$@" >/dev/null 2>&1; do
        if (( SECONDS >= deadline )); then
            echo "FAIL: timed out waiting for: $desc" >&2
            echo "--- sig-a agent log ---" >&2; tail -50 /tmp/scion-agent-test-a/agent.log >&2 || true
            echo "--- sig-b agent log ---" >&2; tail -50 /tmp/scion-agent-test-b/agent.log >&2 || true
            exit 1
        fi
        sleep 1
    done
    echo "PASS: $desc"
}

# --- assertions --------------------------------------------------------------
wait_for "scion0 exists in sig-a" "$TIMEOUT" ip -n sig-a link show scion0
wait_for "scion0 exists in sig-b" "$TIMEOUT" ip -n sig-b link show scion0
wait_for "sig-a has route to peer prefix $PREFIX_B" "$TIMEOUT" \
    sh -c "ip -n sig-a route show $PREFIX_B | grep -q scion0"
wait_for "sig-b has route to peer prefix $PREFIX_A" "$TIMEOUT" \
    sh -c "ip -n sig-b route show $PREFIX_A | grep -q scion0"
wait_for "ping sig-a -> sig-b across the tunnel" "$TIMEOUT" \
    ip netns exec sig-a ping -c1 -W2 -I "$LOOP_A" "$LOOP_B"
wait_for "ping sig-b -> sig-a across the tunnel" "$TIMEOUT" \
    ip netns exec sig-b ping -c1 -W2 -I "$LOOP_B" "$LOOP_A"

# Teardown: killing an agent removes its tun.
kill "${pids[0]}" 2>/dev/null || true
wait "${pids[0]}" 2>/dev/null || true
wait_for "scion0 removed from sig-a after agent shutdown" 15 \
    sh -c "! ip -n sig-a link show scion0"

echo "agent_test.sh: all checks passed"
