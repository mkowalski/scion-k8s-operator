#!/usr/bin/env bash
# End-to-end test suite for scion-k8s-operator on OpenShift.
#
# Runs against a real cluster (KUBECONFIG / oc login) plus an out-of-cluster
# dev SCION AS and registrar. See test/e2e/README.md for prerequisites and
# the full environment variable table.
set -euo pipefail

NAMESPACE=scion-system
OPERATOR_DEPLOY=scion-operator
AGENT_DS=scion-node-agent
TUN_NAME=${TUN_NAME:-scion0}
REMOTE_TCP_PORT=${REMOTE_TCP_PORT:-18080}
NON_SCION_IP=${NON_SCION_IP:-1.1.1.1}
# fedora-toolbox ships iputils ping that works unprivileged (ICMP datagram
# sockets); ubi-minimal/ubi/fedora base images have no ping, busybox ping
# needs raw sockets. Verified locally with podman.
TEST_IMAGE=${TEST_IMAGE:-registry.fedoraproject.org/fedora-toolbox:latest}
TEST_POD=scion-e2e-ping

DEFAULT_PHASES="deploy configure assert_agents assert_dataplane assert_registration churn undeploy"
PHASES=${PHASES:-$DEFAULT_PHASES}

CURRENT_PHASE=""

log()  { printf '%s [e2e] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
ok()   { printf 'OK %s\n' "$1"; }
skip() { printf 'SKIP %s: %s\n' "$1" "$2"; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

dump_diagnostics() {
    printf '%s\n' '--- diagnostics (best-effort) ---' >&2
    oc -n "$NAMESPACE" get pods -o wide >&2 || true
    oc get scionnetwork cluster -o yaml 2>/dev/null | tail -40 >&2 || true
    oc -n "$NAMESPACE" get events --sort-by=.lastTimestamp 2>/dev/null | tail -20 >&2 || true
    local agent_pod
    agent_pod=$(oc -n "$NAMESPACE" get pods -l app="$AGENT_DS" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    if [ -n "$agent_pod" ]; then
        printf '%s\n' "--- logs $agent_pod (last 30 lines) ---" >&2
        oc -n "$NAMESPACE" logs "$agent_pod" --tail=30 >&2 || true
    fi
}

on_exit() {
    local rc=$?
    if [ "$rc" -ne 0 ] && [ -n "$CURRENT_PHASE" ]; then
        printf 'FAILED phase: %s\n' "$CURRENT_PHASE" >&2
        dump_diagnostics
    fi
}
trap on_exit EXIT

usage() {
    cat <<EOF
Usage: [ENV...] $0

Required environment (phase-dependent):
  DISCOVERY_URL    bootstrap discovery server URL      (configure)
  REMOTE_ISD_AS    remote ISD-AS to accept, e.g. 1-ff00:0:111  (configure)
  REGISTRAR_URL    registrar base URL, e.g. http://as-host:8642  (configure, assert_registration, churn, undeploy)
  REGISTRAR_TOKEN  registrar bearer token              (configure, assert_registration, churn, undeploy)
  REMOTE_PING_IP   IP behind the remote SIG to ping    (assert_dataplane)
  UNDERLAY_CIDR    Cluster-to-AS transport CIDR         (configure; default: 192.168.111.0/24)
  REMOTE_SSH       SSH destination for the AS host     (assert_dataplane)

Optional:
  REMOTE_TCP_PORT  Remote SCION TCP test port (default: $REMOTE_TCP_PORT)
  NON_SCION_IP     Control destination that must avoid scion0 (default: $NON_SCION_IP)
  TEST_IMAGE       test pod image (default: $TEST_IMAGE)
  TUN_NAME         tun device name (default: scion0)
  PHASES           space-separated subset of phases to run
                   (default: $DEFAULT_PHASES)
  KEEP_OPERATOR    if set to 1, undeploy keeps the operator installed
EOF
}

require_env() {
    local missing=0 v
    for v in "$@"; do
        if [ -z "${!v:-}" ]; then
            printf 'missing required env: %s\n' "$v" >&2
            missing=1
        fi
    done
    if [ "$missing" -ne 0 ]; then
        usage >&2
        exit 1
    fi
}

first_node() {
    oc get nodes -o jsonpath='{.items[0].metadata.name}'
}

node_has_tun() {
    local node=$1
    oc debug "node/$node" -q -- chroot /host ip link show "$TUN_NAME"
}

node_ipv4() {
    local node=$1 address
    while IFS= read -r address; do
        case "$address" in
            *.*) printf '%s\n' "$address"; return 0 ;;
        esac
    done < <(oc get node "$node" -o jsonpath='{range .status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}')
    die "node $node has no IPv4 InternalIP"
}

route_source() {
    local route=$1 token want_next=0
    for token in $route; do
        if [ "$want_next" -eq 1 ]; then
            printf '%s\n' "$token"
            return 0
        fi
        [ "$token" = "src" ] && want_next=1
    done
    die "route has no source address: $route"
}

remote_exec() {
    if [ "$REMOTE_SSH" = "local" ]; then
        "$@"
    else
        # shellcheck disable=SC2029 # arguments intentionally expand locally
        ssh "$REMOTE_SSH" "$@"
    fi
}

# assert_ipv4 NAME VALUE — values interpolated into remote_exec commands are
# re-split by the remote shell; only accept IPv4 literals so unexpected
# cluster/API output cannot inject remote commands.
assert_ipv4() {
    case "$2" in
        *[!0-9.]*|"") die "$1 is not an IPv4 literal: '$2'" ;;
    esac
}

require_source_preserving_host_routing() {
    local routing advertisements routes
    routing=$(oc get network.operator.openshift.io cluster \
        -o jsonpath='{.spec.defaultNetwork.ovnKubernetesConfig.gatewayConfig.routingViaHost}')
    [ "$routing" = "true" ] || \
        die 'OVN-K routingViaHost must be true; set it explicitly with: oc patch network.operator cluster --type=merge -p '\''{"spec":{"defaultNetwork":{"ovnKubernetesConfig":{"gatewayConfig":{"routingViaHost":true}}}}}'\'''

    advertisements=$(oc get network.operator.openshift.io cluster \
        -o jsonpath='{.spec.defaultNetwork.ovnKubernetesConfig.routeAdvertisements}')
    [ "$advertisements" = "Enabled" ] || \
        die "OVN-K routeAdvertisements is '$advertisements', want Enabled for source preservation"

    routes=$(oc get routeadvertisements.k8s.ovn.org -o jsonpath='{range .items[?(@.status.status=="Accepted")]}{.spec.advertisements}{" "}{.spec.networkSelectors[*].networkSelectionType}{"\n"}{end}')
    printf '%s\n' "$routes" | grep -q 'PodNetwork.*DefaultNetwork' || \
        die "no accepted default-network PodNetwork RouteAdvertisements resource"
}

registrar_sigs() {
    curl -fsS -H "Authorization: Bearer $REGISTRAR_TOKEN" "$REGISTRAR_URL/v1/sigs"
}

wait_agents_ready() {
    local timeout=${1:-600} waited=0 desired ready
    while true; do
        desired=$(oc -n "$NAMESPACE" get daemonset "$AGENT_DS" \
            -o jsonpath='{.status.desiredNumberScheduled}' 2>/dev/null || echo "")
        ready=$(oc -n "$NAMESPACE" get daemonset "$AGENT_DS" \
            -o jsonpath='{.status.numberReady}' 2>/dev/null || echo "")
        if [ -n "$desired" ] && [ "$desired" -gt 0 ] && [ "$desired" = "${ready:-}" ]; then
            log "DaemonSet ready: $ready/$desired"
            return 0
        fi
        if [ "$waited" -ge "$timeout" ]; then
            die "DaemonSet $AGENT_DS not ready after ${timeout}s (desired=$desired ready=$ready)"
        fi
        sleep 10
        waited=$((waited + 10))
    done
}

wait_agent_replaced() {
    local node=$1 old_uid=$2 timeout=${3:-600} waited=0 pod uid ready
    while true; do
        pod=$(oc -n "$NAMESPACE" get pods -l app="$AGENT_DS" \
            --field-selector "spec.nodeName=$node" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
        if [ -n "$pod" ]; then
            uid=$(oc -n "$NAMESPACE" get pod "$pod" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
            ready=$(oc -n "$NAMESPACE" get pod "$pod" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
            if [ -n "$uid" ] && [ "$uid" != "$old_uid" ] && [ "$ready" = "True" ]; then
                return 0
            fi
        fi
        if [ "$waited" -ge "$timeout" ]; then
            die "agent on $node was not replaced and Ready after ${timeout}s"
        fi
        sleep 10
        waited=$((waited + 10))
    done
}

check_registration() {
    local sigs node missing=0
    sigs=$(registrar_sigs)
    log "registrar managed set: $sigs"
    while IFS= read -r node; do
        if ! printf '%s' "$sigs" | grep -Fq "\"$node\":"; then
            printf 'node %s missing from registrar managed set\n' "$node" >&2
            missing=1
        fi
    done < <(oc get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
    [ "$missing" -eq 0 ] || die "registration incomplete"
}

# --- phases -----------------------------------------------------------------

deploy() {
    oc apply -k config/manifests
    oc -n "$NAMESPACE" rollout status "deployment/$OPERATOR_DEPLOY" --timeout=5m
    oc -n "$NAMESPACE" wait --for=condition=Available --timeout=5m \
        "deployment/$OPERATOR_DEPLOY"
    ok deploy
}

configure() {
    require_source_preserving_host_routing
    require_env DISCOVERY_URL REMOTE_ISD_AS REGISTRAR_URL REGISTRAR_TOKEN
    oc -n "$NAMESPACE" create secret generic scion-registrar-token \
        --from-literal=token="$REGISTRAR_TOKEN" \
        --dry-run=client -o yaml | oc apply -f -
    oc apply -f - <<EOF
apiVersion: scion.mkowalski.github.io/v1alpha1
kind: ScionNetwork
metadata:
  name: cluster
spec:
  bootstrap:
    mode: url
    discoveryURL: "$DISCOVERY_URL"
  acceptPolicy:
    isdASes:
      - "$REMOTE_ISD_AS"
    # Node IPs share the NIC that carries the SCION underlay in this test
    # topology. Advertising them makes the remote side route all traffic to
    # the nodes -- including the SCION underlay itself -- through the SIG
    # tunnel, creating a routing loop that blackholes probes, discovery, and
    # registrar traffic. Exclude the underlay and advertise pod CIDRs only.
    underlayCIDRs:
      - "${UNDERLAY_CIDR:-192.168.111.0/24}"
  advertisement:
    podCIDR: true
    nodeIP: false
  registrar:
    backend: http
    endpoint: "$REGISTRAR_URL"
    credentialsSecretRef:
      name: scion-registrar-token
EOF
    ok configure
}

assert_agents() {
    wait_agents_ready 600
    oc wait scionnetwork/cluster --for=condition=Available=True --timeout=5m
    local degraded
    degraded=$(oc get scionnetwork cluster \
        -o jsonpath='{.status.conditions[?(@.type=="Degraded")].status}')
    [ "$degraded" = "False" ] || die "ScionNetwork Degraded condition is '$degraded', want False"
    ok assert_agents
}

assert_dataplane() {
    require_env REMOTE_PING_IP REMOTE_SSH
    assert_ipv4 REMOTE_PING_IP "$REMOTE_PING_IP"
    require_source_preserving_host_routing

    log "launching test pod $TEST_POD ($TEST_IMAGE)"
    oc -n "$NAMESPACE" delete pod "$TEST_POD" --ignore-not-found
    oc -n "$NAMESPACE" run "$TEST_POD" --image="$TEST_IMAGE" \
        --restart=Never --command -- sleep 3600
    oc -n "$NAMESPACE" wait --for=condition=Ready --timeout=5m "pod/$TEST_POD"

    local node node_ip host_source pod_ip route normal_route underlay_route capture capture_pid
    node=$(oc -n "$NAMESPACE" get pod "$TEST_POD" -o jsonpath='{.spec.nodeName}')
    pod_ip=$(oc -n "$NAMESPACE" get pod "$TEST_POD" -o jsonpath='{.status.podIP}')
    assert_ipv4 pod_ip "$pod_ip"

    log "checking learned route to $REMOTE_PING_IP on node $node"
    node_has_tun "$node"
    route=$(oc debug "node/$node" -q -- chroot /host ip -4 route get "$REMOTE_PING_IP")
    printf '%s\n' "$route" | grep -Eq "dev $TUN_NAME([[:space:]]|$)" || \
        die "route to $REMOTE_PING_IP does not select $TUN_NAME: $route"
    host_source=$(route_source "$route")
    assert_ipv4 host_source "$host_source"
    if oc debug "node/$node" -q -- chroot /host nft list table inet scion-node-agent >/dev/null 2>&1; then
        die "unexpected scion-node-agent nftables table on $node"
    fi

    normal_route=$(oc debug "node/$node" -q -- chroot /host ip -4 route get "$NON_SCION_IP")
    if printf '%s\n' "$normal_route" | grep -Eq "dev $TUN_NAME([[:space:]]|$)"; then
        die "non-SCION destination $NON_SCION_IP unexpectedly selects $TUN_NAME: $normal_route"
    fi
    node_ip=$(node_ipv4 "$node")
    underlay_route=$(oc -n "$NAMESPACE" exec "$TEST_POD" -- ip -4 route get "$node_ip")
    if printf '%s\n' "$underlay_route" | grep -Eq "dev $TUN_NAME([[:space:]]|$)"; then
        die "cluster underlay destination $node_ip unexpectedly selects $TUN_NAME: $underlay_route"
    fi
    log "checking remote underlay-bypass guard"

    log "outbound over SCION with pod source $pod_ip"
    capture=$(mktemp)
    remote_exec sudo timeout 20 tcpdump -l -n -i sigb -c 1 \
        icmp and src host "$pod_ip" and dst host "$REMOTE_PING_IP" >"$capture" 2>&1 &
    capture_pid=$!
    sleep 2
    oc -n "$NAMESPACE" exec "$TEST_POD" -- ping -c 3 -W 5 "$REMOTE_PING_IP"
    if ! wait "$capture_pid"; then
        cat "$capture" >&2
        rm -f "$capture"
        die "remote SIG did not observe inner ICMP from pod source $pod_ip"
    fi
    grep -Fq "$pod_ip" "$capture" || { cat "$capture" >&2; rm -f "$capture"; die "capture lacks pod source $pod_ip"; }
    rm -f "$capture"

    log "outbound TCP over SCION"
    oc -n "$NAMESPACE" exec "$TEST_POD" -- \
        curl -fsS --connect-timeout 5 "http://$REMOTE_PING_IP:$REMOTE_TCP_PORT/" >/dev/null

    log "host-originated packet retains route-selected source $host_source"
    capture=$(mktemp)
    remote_exec sudo timeout 20 tcpdump -l -n -i sigb -c 1 \
        icmp and src host "$host_source" and dst host "$REMOTE_PING_IP" >"$capture" 2>&1 &
    capture_pid=$!
    sleep 2
    oc debug "node/$node" -q -- chroot /host ping -c 1 -W 2 "$REMOTE_PING_IP" >/dev/null
    if ! wait "$capture_pid"; then
        cat "$capture" >&2
        rm -f "$capture"
        die "remote SIG did not observe host packet from source $host_source"
    fi
    grep -Fq "$host_source" "$capture" || { cat "$capture" >&2; rm -f "$capture"; die "capture lacks host source $host_source"; }
    rm -f "$capture"

    log "inbound over SCION: ping $pod_ip from $REMOTE_SSH"
    remote_exec ping -c 3 -W 5 -I "$REMOTE_PING_IP" "$pod_ip"

    oc -n "$NAMESPACE" delete pod "$TEST_POD" --ignore-not-found
    ok assert_dataplane
}

assert_registration() {
    require_env REGISTRAR_URL REGISTRAR_TOKEN
    check_registration
    ok assert_registration
}

churn() {
    require_env REGISTRAR_URL REGISTRAR_TOKEN REMOTE_PING_IP
    local victim node old_uid
    victim=$(oc -n "$NAMESPACE" get pods -l app="$AGENT_DS" \
        -o jsonpath='{.items[0].metadata.name}')
    node=$(oc -n "$NAMESPACE" get pod "$victim" -o jsonpath='{.spec.nodeName}')
    old_uid=$(oc -n "$NAMESPACE" get pod "$victim" -o jsonpath='{.metadata.uid}')
    log "deleting agent pod $victim (node $node)"
    oc -n "$NAMESPACE" delete pod "$victim" --wait=true
    wait_agent_replaced "$node" "$old_uid" 600
    wait_agents_ready 600
    check_registration
    log "re-checking learned route on $node"
    node_has_tun "$node"
    oc debug "node/$node" -q -- chroot /host ip -4 route get "$REMOTE_PING_IP" | grep -Eq "dev $TUN_NAME([[:space:]]|$)"
    ok churn
}

undeploy() {
    require_env REGISTRAR_URL REGISTRAR_TOKEN REMOTE_PING_IP
    local node route
    node=$(first_node)
    oc delete scionnetwork cluster --ignore-not-found --wait=true
    log "waiting for DaemonSet $AGENT_DS to disappear"
    local waited=0
    while oc -n "$NAMESPACE" get daemonset "$AGENT_DS" >/dev/null 2>&1; do
        if [ "$waited" -ge 300 ]; then
            die "DaemonSet $AGENT_DS still present after 300s"
        fi
        sleep 10
        waited=$((waited + 10))
    done
    log "checking registrar managed set is empty"
    local sigs
    sigs=$(registrar_sigs)
    case "$sigs" in
        '{}'|'{}'$'\n'|null|'') ;;
        *) die "registrar managed set not empty after undeploy: $sigs" ;;
    esac
    log "checking $TUN_NAME is gone on node $node"
    if node_has_tun "$node" >/dev/null 2>&1; then
        die "$TUN_NAME still present on node $node after undeploy"
    fi
    route=$(oc debug "node/$node" -q -- chroot /host ip -4 route get "$REMOTE_PING_IP" 2>/dev/null || true)
    if printf '%s\n' "$route" | grep -Eq "dev $TUN_NAME([[:space:]]|$)"; then
        die "route to $REMOTE_PING_IP still selects $TUN_NAME after undeploy: $route"
    fi
    if [ "${KEEP_OPERATOR:-0}" != "1" ]; then
        oc delete -k config/manifests
    else
        log "KEEP_OPERATOR=1: leaving operator installed"
        oc -n "$NAMESPACE" delete secret scion-registrar-token --ignore-not-found || true
    fi
    ok undeploy
}

# --- main -------------------------------------------------------------------

main() {
    if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
        usage
        exit 0
    fi
    command -v oc >/dev/null || die "oc CLI not found in PATH"
    local phase
    for phase in $PHASES; do
        case "$phase" in
            deploy|configure|assert_agents|assert_dataplane|assert_registration|churn|undeploy) ;;
            *) die "unknown phase: $phase" ;;
        esac
    done
    for phase in $PHASES; do
        CURRENT_PHASE=$phase
        log "=== phase: $phase ==="
        "$phase"
    done
    log "all phases passed"
}

main "$@"
