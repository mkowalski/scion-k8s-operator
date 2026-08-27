package kube

// Calico IPAM does not populate node.spec.podCIDR(s): per-node pod prefixes
// are /26 blocks allocated dynamically and recorded in the cluster-scoped
// crd.projectcalico.org/v1 BlockAffinity objects (one per node+block).
// RBAC required: get/list on blockaffinities.crd.projectcalico.org.

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var blockAffinityGVR = schema.GroupVersionResource{
	Group: "crd.projectcalico.org", Version: "v1", Resource: "blockaffinities",
}

// CalicoPodCIDRs returns the Calico IPAM block CIDRs affine to the named
// node: confirmed, not marked deleted. The caller treats absence of the
// BlockAffinity API as "not a Calico cluster" (typed NotFound error from
// the dynamic client).
//
// Note: Calico allocates additional blocks as a node's pod count grows;
// callers advertising these prefixes should re-resolve them periodically
// (see the agent's refresh loop) rather than assume the startup set is
// final.
func CalicoPodCIDRs(ctx context.Context, dc dynamic.Interface, node string) ([]string, error) {
	list, err := dc.Resource(blockAffinityGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list calico block affinities: %w", err)
	}
	var cidrs []string
	for i := range list.Items {
		item := &list.Items[i]
		owner, _, _ := unstructured.NestedString(item.Object, "spec", "node")
		if owner != node {
			continue
		}
		state, _, _ := unstructured.NestedString(item.Object, "spec", "state")
		if state != "confirmed" {
			continue
		}
		// spec.deleted is a stringified bool in the Calico CRD.
		if deleted, _, _ := unstructured.NestedString(item.Object, "spec", "deleted"); deleted == "true" {
			continue
		}
		if cidr, _, _ := unstructured.NestedString(item.Object, "spec", "cidr"); cidr != "" {
			cidrs = append(cidrs, cidr)
		}
	}
	return cidrs, nil
}
