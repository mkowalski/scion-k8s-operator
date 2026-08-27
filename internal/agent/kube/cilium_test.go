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

func ciliumNode(name string, podCIDRs []any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNode",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"ipam": map[string]any{"podCIDRs": podCIDRs}},
	}}
}

func TestCiliumPodCIDRs(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{ciliumNodeGVR: "CiliumNodeList"},
		ciliumNode("n1", []any{"10.10.0.0/24", "10.10.1.0/24"}),
	)
	got, err := CiliumPodCIDRs(context.Background(), dc, "n1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.10.0.0/24", "10.10.1.0/24"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CiliumPodCIDRs = %v, want %v", got, want)
	}

	// CRD present, node entry absent (cilium agent still starting): empty
	// result, no error — the caller stays in dynamic-IPAM refresh mode.
	got, err = CiliumPodCIDRs(context.Background(), dc, "absent")
	if err != nil || len(got) != 0 {
		t.Fatalf("missing CiliumNode: got %v err %v, want empty and nil", got, err)
	}
}
