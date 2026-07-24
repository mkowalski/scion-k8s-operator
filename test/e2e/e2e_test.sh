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
    printf '--- diagnostics (best-effort) ---\n' >&2
    oc -n "$NAMESPACE" get pods -o wide >&2 || true
    oc get scionnetwork cluster -o yaml 2>/dev/null | tail -40 >&2 || true
    oc -n "$NAMESPACE" get events --sort-by=.lastTimestamp 2>/dev/null | tail -20 >&2 || true
    local agent_pod
    agent_pod=$(oc -n "$NAMESPACE" get pods -l app="$AGENT_DS" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    if [ -n "$agent_pod" ]; then
        printf '--- logs %s (last 30 lines) ---\n' "$agent_pod" >&2
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

Optional:
  REMOTE_SSH       ssh destination on the remote side for the inbound
                   ping check (e.g. user@as-host); skipped if unset
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
  # tunnel, creating a routing loop that blackholes probe replies and
  # control-plane discovery. Advertise pod CIDRs only.
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
    local avail
    avail=$(oc get scionnetwork cluster \
        -o jsonpath='{.status.conditions[?(@.type=="Available")].status}')
    [ "$avail" = "True" ] || die "ScionNetwork Available condition is '$avail', want True"
    ok assert_agents
}

assert_dataplane() {
    require_env REMOTE_PING_IP
    local node
    node=$(first_node)
    log "checking $TUN_NAME on node $node"
    node_has_tun "$node"

    log "launching test pod $TEST_POD ($TEST_IMAGE)"
    oc -n "$NAMESPACE" delete pod "$TEST_POD" --ignore-not-found
    oc -n "$NAMESPACE" run "$TEST_POD" --image="$TEST_IMAGE" \
        --restart=Never --command -- sleep 3600
    oc -n "$NAMESPACE" wait --for=condition=Ready --timeout=5m "pod/$TEST_POD"

    log "outbound: ping $REMOTE_PING_IP from test pod"
    oc -n "$NAMESPACE" exec "$TEST_POD" -- ping -c 3 -W 5 "$REMOTE_PING_IP"

    if [ -n "${REMOTE_SSH:-}" ]; then
        local pod_ip
        pod_ip=$(oc -n "$NAMESPACE" get pod "$TEST_POD" -o jsonpath='{.status.podIP}')
        log "inbound: ping $pod_ip from $REMOTE_SSH"
        ssh "$REMOTE_SSH" ping -c 3 -W 5 "$pod_ip"
    else
        skip "assert_dataplane(inbound)" "REMOTE_SSH not set; inbound path not verified"
    fi

    oc -n "$NAMESPACE" delete pod "$TEST_POD" --ignore-not-found
    ok assert_dataplane
}

assert_registration() {
    require_env REGISTRAR_URL REGISTRAR_TOKEN
    check_registration
    ok assert_registration
}

churn() {
    require_env REGISTRAR_URL REGISTRAR_TOKEN
    local victim node
    victim=$(oc -n "$NAMESPACE" get pods -l app="$AGENT_DS" \
        -o jsonpath='{.items[0].metadata.name}')
    node=$(oc -n "$NAMESPACE" get pod "$victim" -o jsonpath='{.spec.nodeName}')
    log "deleting agent pod $victim (node $node)"
    oc -n "$NAMESPACE" delete pod "$victim" --wait=true
    wait_agents_ready 600
    check_registration
    log "re-checking $TUN_NAME on node $node"
    node_has_tun "$node"
    ok churn
}

undeploy() {
    require_env REGISTRAR_URL REGISTRAR_TOKEN
    local node
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
