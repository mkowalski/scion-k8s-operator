package controller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mkowalski/scion-k8s-operator/internal/operator/registrar"
)

func node(name string, labels map[string]string, internalIP string) corev1.Node {
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	if internalIP != "" {
		n.Status.Addresses = []corev1.NodeAddress{
			{Type: corev1.NodeHostName, Address: name},
			{Type: corev1.NodeInternalIP, Address: internalIP},
		}
	} else {
		n.Status.Addresses = []corev1.NodeAddress{
			{Type: corev1.NodeHostName, Address: name},
		}
	}
	return n
}

func TestNodesToSIGs(t *testing.T) {
	edge := map[string]string{"role": "edge"}
	tests := []struct {
		name     string
		nodes    []corev1.Node
		selector map[string]string
		want     []registrar.SIG
	}{
		{
			name:  "all nodes when selector empty, sorted by name",
			nodes: []corev1.Node{node("b", nil, "10.0.0.2"), node("a", nil, "10.0.0.1")},
			want: []registrar.SIG{
				{Name: "a", CtrlAddr: "10.0.0.1:30256", DataAddr: "10.0.0.1:30056"},
				{Name: "b", CtrlAddr: "10.0.0.2:30256", DataAddr: "10.0.0.2:30056"},
			},
		},
		{
			name: "selector filters",
			nodes: []corev1.Node{
				node("edge1", edge, "10.0.0.1"),
				node("worker", map[string]string{"role": "worker"}, "10.0.0.2"),
			},
			selector: edge,
			want: []registrar.SIG{
				{Name: "edge1", CtrlAddr: "10.0.0.1:30256", DataAddr: "10.0.0.1:30056"},
			},
		},
		{
			name:  "node without InternalIP skipped",
			nodes: []corev1.Node{node("noip", nil, ""), node("ok", nil, "10.0.0.9")},
			want: []registrar.SIG{
				{Name: "ok", CtrlAddr: "10.0.0.9:30256", DataAddr: "10.0.0.9:30056"},
			},
		},
		{
			name: "no matches yields empty",
			nodes: []corev1.Node{
				node("worker", map[string]string{"role": "worker"}, "10.0.0.2"),
			},
			selector: edge,
			want:     nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nodesToSIGs(tc.nodes, tc.selector)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("nodesToSIGs = %+v, want %+v", got, tc.want)
			}
		})
	}
}
