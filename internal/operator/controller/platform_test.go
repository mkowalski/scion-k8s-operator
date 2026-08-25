package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/mkowalski/scion-k8s-operator/api/v1alpha1"
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
			if tc.messageSubstring != "" && !strings.Contains(got.Message, tc.messageSubstring) {
				t.Fatalf("message %q does not contain %q", got.Message, tc.messageSubstring)
			}
		})
	}
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

// newPlatformFakeClient builds a fake client that knows the optional
// operator.openshift.io Network (and, when registerRA is true, the
// k8s.ovn.org RouteAdvertisements) kinds as unstructured types. Leaving a
// kind unregistered makes Get/List return a NoMatch error, mirroring a
// cluster where the CRD is absent.
func newPlatformFakeClient(t *testing.T, registerRA bool, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: "operator.openshift.io", Version: "v1", Kind: "Network",
	}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: "operator.openshift.io", Version: "v1", Kind: "NetworkList",
	}, &unstructured.UnstructuredList{})
	if registerRA {
		scheme.AddKnownTypeWithName(schema.GroupVersionKind{
			Group: "k8s.ovn.org", Version: "v1", Kind: "RouteAdvertisements",
		}, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(schema.GroupVersionKind{
			Group: "k8s.ovn.org", Version: "v1", Kind: "RouteAdvertisementsList",
		}, &unstructured.UnstructuredList{})
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithInterceptorFuncs(funcs).
		Build()
}

func TestDetectPodEgressPlatform(t *testing.T) {
	networkGR := schema.GroupResource{Group: "operator.openshift.io", Resource: "networks"}
	defaultSelector := []any{map[string]any{"networkSelectionType": "DefaultNetwork"}}
	localGatewayNetwork := func(routeAdvertisements string) *unstructured.Unstructured {
		n := networkObject("OVNKubernetes", true)
		if routeAdvertisements != "" {
			if err := unstructured.SetNestedField(n.Object, routeAdvertisements,
				"spec", "defaultNetwork", "ovnKubernetesConfig", "routeAdvertisements"); err != nil {
				t.Fatal(err)
			}
		}
		return n
	}
	forbiddenGet := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if obj.GetObjectKind().GroupVersionKind().Group == "operator.openshift.io" {
				return apierrors.NewForbidden(networkGR, key.Name, errors.New("rbac denied"))
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	brokenGet := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if obj.GetObjectKind().GroupVersionKind().Group == "operator.openshift.io" {
				return apierrors.NewInternalError(errors.New("apiserver on fire"))
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	forbiddenList := interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Group == "k8s.ovn.org" {
				return apierrors.NewForbidden(schema.GroupResource{Group: "k8s.ovn.org", Resource: "routeadvertisements"}, "", errors.New("rbac denied"))
			}
			return c.List(ctx, list, opts...)
		},
	}
	noMatchList := interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Group == "k8s.ovn.org" {
				return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "k8s.ovn.org", Kind: "RouteAdvertisements"}}
			}
			return c.List(ctx, list, opts...)
		},
	}
	acceptedRA := routeAdvertisement("Accepted", []any{"PodNetwork"}, defaultSelector)
	rejectedRA := routeAdvertisement("Rejected", []any{"PodNetwork"}, defaultSelector)

	tests := []struct {
		name             string
		client           client.Client
		wantStatus       metav1.ConditionStatus
		wantReason       string
		messageSubstring string
	}{
		{
			name:       "network CR absent",
			client:     newPlatformFakeClient(t, false, interceptor.Funcs{}),
			wantStatus: metav1.ConditionUnknown,
			wantReason: "PlatformUnverified",
		},
		{
			name:       "network get forbidden",
			client:     newPlatformFakeClient(t, false, forbiddenGet, localGatewayNetwork("Enabled")),
			wantStatus: metav1.ConditionUnknown,
			wantReason: "PlatformUnverified",
		},
		{
			name:             "network get transient error",
			client:           newPlatformFakeClient(t, false, brokenGet, localGatewayNetwork("Enabled")),
			wantStatus:       metav1.ConditionUnknown,
			wantReason:       "PlatformDetectionFailed",
			messageSubstring: "read OpenShift network operator config",
		},
		{
			name:             "route advertisements not enabled",
			client:           newPlatformFakeClient(t, false, interceptor.Funcs{}, localGatewayNetwork("")),
			wantStatus:       metav1.ConditionFalse,
			wantReason:       "SourcePreservationDisabled",
			messageSubstring: "routeAdvertisements must be Enabled",
		},
		{
			name:             "route advertisements API absent",
			client:           newPlatformFakeClient(t, false, noMatchList, localGatewayNetwork("Enabled")),
			wantStatus:       metav1.ConditionFalse,
			wantReason:       "SourcePreservationDisabled",
			messageSubstring: "API is not available",
		},
		{
			name:       "route advertisements list forbidden",
			client:     newPlatformFakeClient(t, true, forbiddenList, localGatewayNetwork("Enabled")),
			wantStatus: metav1.ConditionUnknown,
			wantReason: "PlatformUnverified",
		},
		{
			name:             "no accepted default advertisement",
			client:           newPlatformFakeClient(t, true, interceptor.Funcs{}, localGatewayNetwork("Enabled"), &rejectedRA),
			wantStatus:       metav1.ConditionFalse,
			wantReason:       "SourcePreservationDisabled",
			messageSubstring: "no accepted RouteAdvertisements",
		},
		{
			name:             "source preservation verified",
			client:           newPlatformFakeClient(t, true, interceptor.Funcs{}, localGatewayNetwork("Enabled"), &acceptedRA),
			wantStatus:       metav1.ConditionTrue,
			wantReason:       "HostRoutingEnabled",
			messageSubstring: "pod source preservation",
		},
		{
			name:             "shared gateway",
			client:           newPlatformFakeClient(t, false, interceptor.Funcs{}, networkObject("OVNKubernetes", false)),
			wantStatus:       metav1.ConditionFalse,
			wantReason:       "HostRoutingDisabled",
			messageSubstring: "routingViaHost=true",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &ScionNetworkReconciler{Client: tc.client}
			got := r.detectPodEgressPlatform(context.Background())
			if got.Status != tc.wantStatus || got.Reason != tc.wantReason {
				t.Fatalf("platform = %+v, want status=%s reason=%s", got, tc.wantStatus, tc.wantReason)
			}
			if tc.messageSubstring != "" && !strings.Contains(got.Message, tc.messageSubstring) {
				t.Fatalf("message %q does not contain %q", got.Message, tc.messageSubstring)
			}
		})
	}
}

func TestRouteAdvertisementAcceptedConditions(t *testing.T) {
	defaultSelector := []any{map[string]any{"networkSelectionType": "DefaultNetwork"}}
	conditionRA := func(condType, condStatus string) unstructured.Unstructured {
		ra := routeAdvertisement("", []any{"PodNetwork"}, defaultSelector)
		ra.Object["status"] = map[string]any{
			"conditions": []any{map[string]any{"type": condType, "status": condStatus}},
		}
		return ra
	}
	if !hasAcceptedDefaultPodAdvertisement([]unstructured.Unstructured{conditionRA("Accepted", "True")}) {
		t.Fatal("conditions-based Accepted=True was not detected")
	}
	for _, ra := range []unstructured.Unstructured{
		conditionRA("Accepted", "False"),
		conditionRA("Ready", "True"),
	} {
		if hasAcceptedDefaultPodAdvertisement([]unstructured.Unstructured{ra}) {
			t.Fatalf("unexpected acceptance for %#v", ra.Object["status"])
		}
	}
}
