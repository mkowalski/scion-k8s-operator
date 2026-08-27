package kube

import (
	"context"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func blockAffinity(name, node, cidr, state, deleted string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "crd.projectcalico.org/v1",
		"kind":       "BlockAffinity",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"node": node, "cidr": cidr, "state": state, "deleted": deleted,
		},
	}}
}

func TestCalicoPodCIDRs(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{blockAffinityGVR: "BlockAffinityList"},
		blockAffinity("n1-1", "n1", "10.244.0.0/26", "confirmed", "false"),
		blockAffinity("n1-2", "n1", "10.244.0.64/26", "confirmed", "false"),
		blockAffinity("n1-pending", "n1", "10.244.0.128/26", "pending", "false"),
		blockAffinity("n1-deleted", "n1", "10.244.0.192/26", "confirmed", "true"),
		blockAffinity("n2-1", "n2", "10.244.1.0/26", "confirmed", "false"),
	)
	got, err := CalicoPodCIDRs(context.Background(), dc, "n1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.244.0.0/26", "10.244.0.64/26"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CalicoPodCIDRs = %v, want %v", got, want)
	}
}
