package kube

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNodeInfo(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0"},
		Spec:       corev1.NodeSpec{PodCIDRs: []string{"10.128.2.0/23"}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "192.0.2.11"},
		}},
	}
	cs := fake.NewSimpleClientset(node)
	info, err := GetNodeInfo(context.Background(), cs, "worker-0")
	if err != nil {
		t.Fatal(err)
	}
	if info.PodCIDRs[0] != "10.128.2.0/23" || info.InternalIP != "192.0.2.11" {
		t.Fatalf("got %+v", info)
	}
}

func TestNodeInfoOVNAnnotationList(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "worker-1",
			Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":["10.128.2.0/23","fd01::/64"]}`},
		},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "192.0.2.12"},
		}},
	}
	cs := fake.NewSimpleClientset(node)
	info, err := GetNodeInfo(context.Background(), cs, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.PodCIDRs) != 2 || info.PodCIDRs[0] != "10.128.2.0/23" || info.PodCIDRs[1] != "fd01::/64" {
		t.Fatalf("got %+v", info)
	}
}

func TestNodeInfoOVNAnnotationString(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "worker-2",
			Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"10.128.4.0/23"}`},
		},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "192.0.2.13"},
		}},
	}
	cs := fake.NewSimpleClientset(node)
	info, err := GetNodeInfo(context.Background(), cs, "worker-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.PodCIDRs) != 1 || info.PodCIDRs[0] != "10.128.4.0/23" {
		t.Fatalf("got %+v", info)
	}
}

func TestNodeInfoNoInternalIP(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-3"},
		Spec:       corev1.NodeSpec{PodCIDRs: []string{"10.128.6.0/23"}},
	}
	cs := fake.NewSimpleClientset(node)
	if _, err := GetNodeInfo(context.Background(), cs, "worker-3"); err == nil {
		t.Fatal("expected error for missing InternalIP")
	}
}

func TestNodeInfoSingularPodCIDR(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-4"},
		Spec:       corev1.NodeSpec{PodCIDR: "10.128.8.0/23"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "192.0.2.14"},
		}},
	}
	cs := fake.NewSimpleClientset(node)
	info, err := GetNodeInfo(context.Background(), cs, "worker-4")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.PodCIDRs) != 1 || info.PodCIDRs[0] != "10.128.8.0/23" {
		t.Fatalf("got %+v", info)
	}
}
