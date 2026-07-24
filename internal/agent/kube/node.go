// Package kube reads this node's identity from the Kubernetes API.
// RBAC required: get on nodes (cluster-scoped), nothing else.
package kube

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// NodeInfo holds the per-node data the agent advertises into SCION.
type NodeInfo struct {
	PodCIDRs   []string
	InternalIP string
}

// GetNodeInfo fetches the named node and extracts its pod CIDRs and
// InternalIP. Pod CIDRs are taken from spec.podCIDRs, falling back to the
// singular spec.podCIDR, then to the OVN-Kubernetes node-subnets annotation
// (OpenShift default CNI leaves spec.podCIDRs empty).
func GetNodeInfo(ctx context.Context, cs kubernetes.Interface, name string) (NodeInfo, error) {
	node, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return NodeInfo{}, fmt.Errorf("get node %s: %w", name, err)
	}
	info := NodeInfo{PodCIDRs: node.Spec.PodCIDRs}
	if len(info.PodCIDRs) == 0 && node.Spec.PodCIDR != "" {
		info.PodCIDRs = []string{node.Spec.PodCIDR}
	}
	if len(info.PodCIDRs) == 0 {
		info.PodCIDRs = ovnSubnets(node.Annotations["k8s.ovn.org/node-subnets"])
	}
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			info.InternalIP = a.Address
			break
		}
	}
	if info.InternalIP == "" {
		return info, fmt.Errorf("node %s has no InternalIP", name)
	}
	return info, nil
}

// ovnSubnets parses the k8s.ovn.org/node-subnets annotation; the "default"
// network value is a JSON list of CIDRs in current OVN-K, a plain string in
// older versions. Returns nil if absent or unparsable.
func ovnSubnets(raw string) []string {
	if raw == "" {
		return nil
	}
	var networks map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &networks); err != nil {
		return nil
	}
	def, ok := networks["default"]
	if !ok {
		return nil
	}
	var list []string
	if err := json.Unmarshal(def, &list); err == nil {
		return list
	}
	var single string
	if err := json.Unmarshal(def, &single); err == nil && single != "" {
		return []string{single}
	}
	return nil
}
