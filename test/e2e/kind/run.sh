#!/usr/bin/env bash
# kind-based e2e for scion-k8s-operator on vanilla Kubernetes (default CNI:
# kindnetd). Brings up a kind cluster plus a single-container two-AS SCION
# topology on the kind container network, deploys the operator built from
# the working tree, and asserts TCP connectivity in both directions through
# the SCION tunnel with preserved pod sources.
#
# Requirements: docker (or CONTAINER_ENGINE=podman with
# KIND_EXPERIMENTAL_PROVIDER=podman), kind, kubectl, kustomize (bin/ from
# `make kustomize` is used when present).
#
# Environment:
#   CONTAINER_ENGINE  docker (default) | podman
#   KEEP=1            keep the cluster and AS container on exit
set -euo pipefail

ENGINE=${CONTAINER_ENGINE:-docker}
CLUSTER=scion-e2e
AS_NAME=scion-e2e-as
REMOTE_PREFIX=192.168.100.0/24
REMOTE_PING_IP=192.168.100.1
REMOTE_TCP_PORT=18080
REMOTE_ISD_AS=1-ff00:0:111
CLUSTER_PREFIXES=10.244.0.0/16
NAMESPACE=scion-system
TEST_IMAGE=docker.io/library/python:3.12-alpine

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"
log() { printf '%s [kind-e2e] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

cleanup() {
    local rc=$?
    if [ "$rc" -ne 0 ]; then
        printf '%s\n' '--- diagnostics ---' >&2
        kubectl -n "$NAMESPACE" get pods -o wide >&2 || true
        kubectl get scionnetwork cluster -o yaml 2>/dev/null | tail -30 >&2 || true
        kubectl -n "$NAMESPACE" logs ds/scion-node-agent --tail=40 >&2 || true
        kubectl -n "$NAMESPACE" logs deploy/scion-operator --tail=20 >&2 || true
        "$ENGINE" logs --tail 60 "$AS_NAME" >&2 || true
    fi
    if [ "${KEEP:-0}" != "1" ]; then
        kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
        "$ENGINE" rm -f "$AS_NAME" >/dev/null 2>&1 || true
    fi
    exit "$rc"
}
trap cleanup EXIT

# --- build images --------------------------------------------------------------
log "building images (operator, agent, scion-as)"
"$ENGINE" build -q -f "$repo/build/Dockerfile.operator" -t quay.io/mkowalski/scion-operator:e2e "$repo"
"$ENGINE" build -q -f "$repo/build/Dockerfile.agent" -t quay.io/mkowalski/scion-node-agent:e2e "$repo"
"$ENGINE" build -q -f "$here/Dockerfile.scion-as" -t scion-as:e2e "$repo"

# --- cluster -------------------------------------------------------------------
log "creating kind cluster"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --config "$here/kind-config.yaml" --wait 3m
# image-archive works identically for docker and podman image stores; one
# archive per image (podman treats extra args to `save` as additional tags
# of the first image, silently collapsing them onto one image ID).
for img in quay.io/mkowalski/scion-operator:e2e quay.io/mkowalski/scion-node-agent:e2e; do
    rm -f /tmp/scion-e2e-image.tar
    "$ENGINE" save -o /tmp/scion-e2e-image.tar "$img"
    kind load image-archive --name "$CLUSTER" /tmp/scion-e2e-image.tar
done
rm -f /tmp/scion-e2e-image.tar

# --- AS topology on the kind network --------------------------------------------
REGISTRAR_TOKEN=$(head -c16 /dev/urandom | base64 | tr -d '=+/')
log "starting SCION AS container"
"$ENGINE" rm -f "$AS_NAME" >/dev/null 2>&1 || true
"$ENGINE" run -d --name "$AS_NAME" --network kind \
    --cap-add NET_ADMIN --device /dev/net/tun \
    -e REGISTRAR_TOKEN="$REGISTRAR_TOKEN" \
    -e CLUSTER_PREFIXES="$CLUSTER_PREFIXES" \
    -e REMOTE_PREFIX="$REMOTE_PREFIX" \
    scion-as:e2e >/dev/null
AS_IP=$("$ENGINE" inspect -f '{{ (index .NetworkSettings.Networks "kind").IPAddress }}' "$AS_NAME")
[ -n "$AS_IP" ] || die "could not determine AS container IP"
# The node-to-AS transport network (docker: 172.18.0.0/16, podman: 10.89.x.0/24).
UNDERLAY_CIDR=$("$ENGINE" network inspect kind --format '{{range .IPAM.Config}}{{.Subnet}} {{end}}' 2>/dev/null | tr ' ' '\n' | grep -E '^[0-9]+\.' | head -1 || true)
if [ -z "$UNDERLAY_CIDR" ]; then # podman network inspect format
    UNDERLAY_CIDR=$("$ENGINE" network inspect kind --format '{{range .Subnets}}{{.Subnet}} {{end}}' | tr ' ' '\n' | grep -E '^[0-9]+\.' | head -1)
fi
[ -n "$UNDERLAY_CIDR" ] || die "could not determine kind network subnet"
log "AS at $AS_IP (underlay $UNDERLAY_CIDR)"

for _ in $(seq 24); do
    curl -fsS --max-time 3 "http://$AS_IP:8041/topology" >/dev/null 2>&1 && break
    sleep 5
done
curl -fsS "http://$AS_IP:8041/topology" | grep -q "1-ff00:0:110" || die "discovery not serving"
curl -fsS -H "Authorization: Bearer $REGISTRAR_TOKEN" "http://$AS_IP:8642/v1/sigs" >/dev/null || die "registrar not serving"

# --- masquerade exclusion --------------------------------------------------------
# kindnetd masquerades pod->external traffic; traffic steered into scion0
# must keep its pod source (and MASQUERADE onto an addressless tun would
# break entirely). Exempt the remote SCION prefix on every node.
for node in $(kind get nodes --name "$CLUSTER"); do
    if "$ENGINE" exec "$node" iptables -t nat -S KIND-MASQ-AGENT >/dev/null 2>&1; then
        "$ENGINE" exec "$node" iptables -t nat -I KIND-MASQ-AGENT 1 -d "$REMOTE_PREFIX" \
            -m comment --comment "scion-e2e: no masquerade into the SCION tunnel" -j RETURN
    else
        "$ENGINE" exec "$node" iptables -t nat -I POSTROUTING 1 -d "$REMOTE_PREFIX" \
            -m comment --comment "scion-e2e: no masquerade into the SCION tunnel" -j ACCEPT
    fi
done

# --- operator --------------------------------------------------------------------
log "deploying operator"
# shellcheck disable=SC2012
KUSTOMIZE=$(ls "$repo"/bin/kustomize-* 2>/dev/null | head -1 || true)
if [ -z "$KUSTOMIZE" ]; then
    make -C "$repo" kustomize >/dev/null
    # shellcheck disable=SC2012
    KUSTOMIZE=$(ls "$repo"/bin/kustomize-* | head -1)
fi
"$KUSTOMIZE" build --load-restrictor LoadRestrictionsNone "$here/manifests" | kubectl apply -f -
kubectl -n "$NAMESPACE" rollout status deploy/scion-operator --timeout=3m

# --- ScionNetwork ------------------------------------------------------------------
log "creating ScionNetwork"
kubectl -n "$NAMESPACE" create secret generic scion-registrar-token \
    --from-literal=token="$REGISTRAR_TOKEN"
kubectl apply -f - <<EOF
apiVersion: scion.mkowalski.github.io/v1alpha1
kind: ScionNetwork
metadata:
  name: cluster
spec:
  agentImage: quay.io/mkowalski/scion-node-agent:e2e
  bootstrap:
    mode: url
    discoveryURL: http://$AS_IP:8041
  acceptPolicy:
    isdASes: ["$REMOTE_ISD_AS"]
    underlayCIDRs: ["$UNDERLAY_CIDR"]
  registrar:
    backend: http
    endpoint: http://$AS_IP:8642
    credentialsSecretRef:
      name: scion-registrar-token
EOF

log "waiting for agents"
for _ in $(seq 24); do
    kubectl -n "$NAMESPACE" get ds scion-node-agent >/dev/null 2>&1 && break
    sleep 5
done
kubectl -n "$NAMESPACE" rollout status ds/scion-node-agent --timeout=4m
kubectl wait --for=condition=Available --timeout=3m scionnetwork/cluster

# The forbidden list must have been derived without OpenShift APIs.
forbidden=$(kubectl -n "$NAMESPACE" get ds scion-node-agent \
    -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="SCION_FORBIDDEN_CIDRS")].value}')
log "derived forbidden CIDRs: $forbidden"
case "$forbidden" in
    *10.244.*) : ;; *) die "pod network missing from derived forbidden CIDRs: $forbidden" ;;
esac
case "$forbidden" in
    *10.96.*) : ;; *) die "service network missing from derived forbidden CIDRs (ServiceCIDR API): $forbidden" ;;
esac

# --- workload + connectivity ---------------------------------------------------------
log "deploying sample workload"
kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: scion-e2e-web
  labels: { app: scion-e2e-web }
spec:
  containers:
    - name: web
      image: $TEST_IMAGE
      command: ["python3", "-m", "http.server", "8000"]
      readinessProbe: { httpGet: { path: /, port: 8000 } }
EOF
kubectl -n "$NAMESPACE" wait --for=condition=Ready --timeout=3m pod/scion-e2e-web
POD_IP=$(kubectl -n "$NAMESPACE" get pod scion-e2e-web -o jsonpath='{.status.podIP}')
NODE=$(kubectl -n "$NAMESPACE" get pod scion-e2e-web -o jsonpath='{.spec.nodeName}')
log "workload pod $POD_IP on $NODE"

log "route to remote must select scion0"
route=$("$ENGINE" exec "$NODE" ip -4 route get "$REMOTE_PING_IP")
echo "$route" | grep -q "dev scion0" || die "route does not select scion0: $route"

log "egress: pod -> remote echo over SCION"
kubectl -n "$NAMESPACE" exec scion-e2e-web -- \
    wget -qO /dev/null -T 10 "http://$REMOTE_PING_IP:$REMOTE_TCP_PORT/" || die "egress request failed"
# Path-conclusive: the echo's access log must show the pod source (no
# masquerade), and the nft guard already dropped anything not from sigb.
"$ENGINE" logs "$AS_NAME" 2>&1 | grep "\[echo\]" | grep -q "$POD_IP" || \
    die "remote echo saw no request from pod source $POD_IP (masqueraded or bypassed?)"

log "inbound: remote AS ($REMOTE_PING_IP) -> pod over SCION"
"$ENGINE" exec "$AS_NAME" curl -fsS --max-time 10 --interface "$REMOTE_PING_IP" \
    -o /dev/null "http://$POD_IP:8000/" || die "inbound request failed"

# --- teardown ---------------------------------------------------------------------
log "undeploy: ScionNetwork deletion must deregister and remove the tun"
kubectl delete scionnetwork cluster --timeout=3m
sigs=$(curl -fsS -H "Authorization: Bearer $REGISTRAR_TOKEN" "http://$AS_IP:8642/v1/sigs")
[ "$sigs" = "{}" ] || die "registrar set not empty after undeploy: $sigs"
# The DaemonSet (and with it the tun) is removed by owner-reference GC,
# which is asynchronous relative to the ScionNetwork deletion.
for _ in $(seq 24); do
    "$ENGINE" exec "$NODE" ip link show scion0 >/dev/null 2>&1 || break
    sleep 5
done
if "$ENGINE" exec "$NODE" ip link show scion0 >/dev/null 2>&1; then
    die "scion0 still present on $NODE after undeploy"
fi

log "ALL CHECKS PASSED"
