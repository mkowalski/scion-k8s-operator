package kube

// Cilium cluster-pool IPAM does not populate node.spec.podCIDR(s): per-node
// pod prefixes are allocated by the cilium-operator and recorded in the
// cluster-scoped cilium.io/v2 CiliumNode object (same name as the Node),
// under spec.ipam.podCIDRs. RBAC required: get/list on
// ciliumnodes.cilium.io.

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var ciliumNodeGVR = schema.GroupVersionResource{
	Group: "cilium.io", Version: "v2", Resource: "ciliumnodes",
}

// CiliumPodCIDRs returns the pod CIDRs allocated to the named node by
// Cilium IPAM. A List is used deliberately: a missing CRD yields a typed
// NotFound ("not a Cilium cluster"), while a present CRD without an entry
// for this node yet (cilium agent still starting) yields an empty result —
// the caller then knows dynamic IPAM is in play and must keep refreshing.
//
// Like Calico blocks, the set can grow at runtime (multi-pool IPAM), so
// callers advertising these prefixes should re-resolve them periodically.
func CiliumPodCIDRs(ctx context.Context, dc dynamic.Interface, node string) ([]string, error) {
	list, err := dc.Resource(ciliumNodeGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list cilium nodes: %w", err)
	}
	for i := range list.Items {
		if list.Items[i].GetName() != node {
			continue
		}
		cidrs, _, _ := unstructured.NestedStringSlice(list.Items[i].Object, "spec", "ipam", "podCIDRs")
		return cidrs, nil
	}
	return nil, nil
}
