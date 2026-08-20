package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func networkObject(networkType string, routingViaHost any) *unstructured.Unstructured {
	gateway := map[string]any{}
	if routingViaHost != nil {
		gateway["routingViaHost"] = routingViaHost
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operator.openshift.io/v1",
		"kind":       "Network",
		"metadata":   map[string]any{"name": "cluster"},
		"spec": map[string]any{
			"defaultNetwork": map[string]any{
				"type": networkType,
				"ovnKubernetesConfig": map[string]any{
					"gatewayConfig": gateway,
				},
			},
		},
	}}
}

func routeAdvertisement(status string, advertisements []any, selectors []any) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "k8s.ovn.org/v1",
		"kind":       "RouteAdvertisements",
		"metadata":   map[string]any{"name": "default"},
		"spec": map[string]any{
			"advertisements":   advertisements,
			"networkSelectors": selectors,
		},
		"status": map[string]any{"status": status},
	}}
}

func TestClassifyPodEgressPlatform(t *testing.T) {
	tests := []struct {
		name             string
		network          *unstructured.Unstructured
		wantStatus       metav1.ConditionStatus
		wantReason       string
		messageSubstring string
	}{
		{
			name:       "local gateway supported",
			network:    networkObject("OVNKubernetes", true),
			wantStatus: metav1.ConditionTrue,
			wantReason: "HostRoutingEnabled",
		},
		{
			name:             "shared gateway unsupported",
			network:          networkObject("OVNKubernetes", false),
			wantStatus:       metav1.ConditionFalse,
			wantReason:       "HostRoutingDisabled",
			messageSubstring: "routingViaHost=true",
		},
		{
			name:       "missing setting defaults unsupported",
			network:    networkObject("OVNKubernetes", nil),
			wantStatus: metav1.ConditionFalse,
			wantReason: "HostRoutingDisabled",
		},
		{
			name:       "different network unknown",
			network:    networkObject("Other", true),
			wantStatus: metav1.ConditionUnknown,
			wantReason: "PlatformUnverified",
		},
		{
			name:       "malformed setting unsupported",
			network:    networkObject("OVNKubernetes", "true"),
			wantStatus: metav1.ConditionFalse,
			wantReason: "HostRoutingDisabled",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyPodEgressPlatform(tc.network)
			if got.Status != tc.wantStatus || got.Reason != tc.wantReason {
				t.Fatalf("classification = %+v, want status=%s reason=%s", got, tc.wantStatus, tc.wantReason)
			}
			if tc.messageSubstring != "" && !contains(got.Message, tc.messageSubstring) {
				t.Fatalf("message %q does not contain %q", got.Message, tc.messageSubstring)
			}
		})
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return substr == ""
}

func TestHasAcceptedDefaultPodAdvertisement(t *testing.T) {
	defaultSelector := []any{map[string]any{"networkSelectionType": "DefaultNetwork"}}
	if !hasAcceptedDefaultPodAdvertisement([]unstructured.Unstructured{
		routeAdvertisement("Accepted", []any{"PodNetwork"}, defaultSelector),
	}) {
		t.Fatal("accepted default PodNetwork advertisement was not detected")
	}
	for _, item := range []unstructured.Unstructured{
		routeAdvertisement("Rejected", []any{"PodNetwork"}, defaultSelector),
		routeAdvertisement("Accepted", []any{"EgressIP"}, defaultSelector),
		routeAdvertisement("Accepted", []any{"PodNetwork"}, []any{map[string]any{"networkSelectionType": "NetworkAttachmentDefinition"}}),
	} {
		if hasAcceptedDefaultPodAdvertisement([]unstructured.Unstructured{item}) {
			t.Fatalf("unexpected source-preservation match for %#v", item.Object)
		}
	}
}
