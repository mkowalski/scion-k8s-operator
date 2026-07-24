package v1alpha1

import (
	"testing"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestAddToScheme(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	gvk := GroupVersion.WithKind("ScionNetwork")
	if !s.Recognizes(gvk) {
		t.Errorf("scheme does not recognize %v", gvk)
	}
	if !s.Recognizes(GroupVersion.WithKind("ScionNetworkList")) {
		t.Errorf("scheme does not recognize ScionNetworkList")
	}
}

func TestScionNetworkDeepCopy(t *testing.T) {
	truth := true
	in := &ScionNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: ScionNetworkSpec{
			Bootstrap: BootstrapSpec{
				Mode:            "url",
				DiscoveryURL:    "https://bootstrap.example",
				RefreshInterval: "1h",
			},
			Advertisement: AdvertisementSpec{PodCIDR: &truth, NodeIP: &truth},
			AcceptPolicy: AcceptPolicySpec{
				ISDASes:        []string{"1-ff00:0:110"},
				ForbiddenCIDRs: []string{"10.0.0.0/8"},
			},
			NodeSelector: map[string]string{"role": "edge"},
		},
		Status: ScionNetworkStatus{
			ISDAS: "1-ff00:0:111",
			Nodes: NodeSummary{Ready: 2, Total: 3, Degraded: []string{"node-c"}},
			Registrar: RegistrarStatus{
				DesiredSIGs: []string{"192.0.2.1:30041"},
			},
		},
	}
	out := in.DeepCopy()
	if !apiequality.Semantic.DeepEqual(in, out) {
		t.Fatal("DeepCopy result differs from original")
	}
	// Mutate the copy; original must be unaffected.
	out.Spec.AcceptPolicy.ISDASes[0] = "changed"
	*out.Spec.Advertisement.PodCIDR = false
	if in.Spec.AcceptPolicy.ISDASes[0] != "1-ff00:0:110" {
		t.Error("DeepCopy shares ISDASes slice with original")
	}
	if *in.Spec.Advertisement.PodCIDR != true {
		t.Error("DeepCopy shares PodCIDR pointer with original")
	}
}
