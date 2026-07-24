package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/mkowalski/scion-k8s-operator/api/v1alpha1"
	"github.com/mkowalski/scion-k8s-operator/internal/operator/registrar"
	"github.com/mkowalski/scion-k8s-operator/internal/operator/render"
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
		name        string
		nodes       []corev1.Node
		selector    map[string]string
		want        []registrar.SIG
		wantSkipped []string
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
			name:  "node without InternalIP skipped and reported",
			nodes: []corev1.Node{node("noip", nil, ""), node("ok", nil, "10.0.0.9")},
			want: []registrar.SIG{
				{Name: "ok", CtrlAddr: "10.0.0.9:30256", DataAddr: "10.0.0.9:30056"},
			},
			wantSkipped: []string{"noip"},
		},
		{
			name: "unselected node without InternalIP not reported",
			nodes: []corev1.Node{
				node("noip-worker", map[string]string{"role": "worker"}, ""),
				node("edge1", edge, "10.0.0.1"),
			},
			selector: edge,
			want: []registrar.SIG{
				{Name: "edge1", CtrlAddr: "10.0.0.1:30256", DataAddr: "10.0.0.1:30056"},
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
			got, skipped := nodesToSIGs(tc.nodes, tc.selector)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("nodesToSIGs sigs = %+v, want %+v", got, tc.want)
			}
			if !reflect.DeepEqual(skipped, tc.wantSkipped) {
				t.Errorf("nodesToSIGs skipped = %v, want %v", skipped, tc.wantSkipped)
			}
		})
	}
}

func TestBackendForHTTPSecretWithoutTokenKey(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: render.Namespace, Name: "creds"},
		Data:       map[string][]byte{"password": []byte("nope")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sec).Build()
	r := &ScionNetworkReconciler{Client: c, Scheme: scheme}

	sn := &v1alpha1.ScionNetwork{}
	sn.Spec.Registrar.Backend = "http"
	sn.Spec.Registrar.Endpoint = "http://as:8642"
	sn.Spec.Registrar.CredentialsSecretRef = &corev1.LocalObjectReference{Name: "creds"}

	_, err := r.backendFor(context.Background(), sn)
	if err == nil || !strings.Contains(err.Error(), `has no "token" key`) {
		t.Fatalf("backendFor = %v, want missing token key error", err)
	}
}

func TestDegradedReason(t *testing.T) {
	tests := []struct {
		name      string
		ready     int32
		total     int32
		regFailed bool
		backend   string
		wantOn    bool
		wantWhy   string
	}{
		{"all healthy", 2, 2, false, "http", false, "UnreadyAgents"},
		{"unready agents only", 1, 2, false, "manual", true, "UnreadyAgents"},
		{"registrar failed, agents ready", 2, 2, true, "http", true, "RegistrarSyncFailed"},
		{"registrar failed, anapaya", 2, 2, true, "anapaya", true, "RegistrarSyncFailed"},
		{"registrar failed but unready agents take precedence", 1, 2, true, "http", true, "UnreadyAgents"},
		{"registrar failed with manual backend does not degrade", 2, 2, true, "manual", false, "UnreadyAgents"},
		{"registrar failed with default backend does not degrade", 2, 2, true, "", false, "UnreadyAgents"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			on, why := degradedReason(tc.ready, tc.total, tc.regFailed, tc.backend)
			if on != tc.wantOn || why != tc.wantWhy {
				t.Errorf("degradedReason = (%v, %q), want (%v, %q)", on, why, tc.wantOn, tc.wantWhy)
			}
		})
	}
}
