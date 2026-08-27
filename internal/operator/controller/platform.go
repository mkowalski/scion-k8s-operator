package controller

import (
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

type podEgressPlatform struct {
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

// detectPodEgressPlatform probes optional OpenShift/OVN-Kubernetes APIs to
// classify whether pod egress preserves source addresses. The probe is
// informational: it must never block the data-plane reconcile, so every
// failure degrades to a condition status instead of an error. Forbidden is
// treated like NotFound/NoMatch (e.g. a stale ClusterRole during upgrade, or
// OVN CRDs present on a cluster where the operator has no read access).
func (r *ScionNetworkReconciler) detectPodEgressPlatform(ctx context.Context) podEgressPlatform {
	network := &unstructured.Unstructured{}
	network.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "operator.openshift.io", Version: "v1", Kind: "Network",
	})
	if err := r.Get(ctx, types.NamespacedName{Name: "cluster"}, network); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || meta.IsNoMatchError(err) {
			return podEgressPlatform{
				Status:  metav1.ConditionUnknown,
				Reason:  "PlatformUnverified",
				Message: r.vanillaPlatformMessage(ctx),
			}
		}
		return platformDetectionFailed(fmt.Errorf("read OpenShift network operator config: %w", err))
	}
	platform := classifyPodEgressPlatform(network)
	if platform.Status != metav1.ConditionTrue {
		return platform
	}
	routeAdvertisements, _, _ := unstructured.NestedString(network.Object,
		"spec", "defaultNetwork", "ovnKubernetesConfig", "routeAdvertisements")
	if routeAdvertisements != "Enabled" {
		return sourcePreservationDisabled("OVN routeAdvertisements must be Enabled to preserve pod source addresses")
	}

	routes := &unstructured.UnstructuredList{}
	routes.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.ovn.org", Version: "v1", Kind: "RouteAdvertisementsList",
	})
	if err := r.List(ctx, routes); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return sourcePreservationDisabled("OVN RouteAdvertisements API is not available")
		}
		if apierrors.IsForbidden(err) {
			return podEgressPlatform{
				Status:  metav1.ConditionUnknown,
				Reason:  "PlatformUnverified",
				Message: "OVN RouteAdvertisements are not readable by the operator",
			}
		}
		return platformDetectionFailed(fmt.Errorf("list OVN RouteAdvertisements: %w", err))
	}
	if !hasAcceptedDefaultPodAdvertisement(routes.Items) {
		return sourcePreservationDisabled("no accepted RouteAdvertisements exports PodNetwork for the default network")
	}
	return podEgressPlatform{
		Status:  metav1.ConditionTrue,
		Reason:  "HostRoutingEnabled",
		Message: "OVN-Kubernetes routes pod egress through the host with pod source preservation",
	}
}

func platformDetectionFailed(err error) podEgressPlatform {
	return podEgressPlatform{
		Status:  metav1.ConditionUnknown,
		Reason:  "PlatformDetectionFailed",
		Message: err.Error(),
	}
}

// vanillaPlatformMessage enriches the PlatformUnverified condition with a
// best-effort CNI hint on non-OpenShift clusters. Source preservation
// cannot be verified there (masquerade exemptions for dynamically learned
// prefixes are not observable through any API), so the status stays
// Unknown, but the message tells the operator of the cluster what to check.
func (r *ScionNetworkReconciler) vanillaPlatformMessage(ctx context.Context) string {
	const base = "OpenShift OVN-Kubernetes gateway configuration is not available"
	if r.apiPresent(ctx, schema.GroupVersionKind{
		Group: "crd.projectcalico.org", Version: "v1", Kind: "IPPoolList",
	}) {
		return base + "; Calico detected — source preservation requires prefixes " +
			"accepted from remote ASes to be covered by a disabled IPPool with " +
			"disableBGPExport (see docs/install.md, Vanilla Kubernetes)"
	}
	if r.apiPresent(ctx, schema.GroupVersionKind{
		Group: "cilium.io", Version: "v2", Kind: "CiliumNodeList",
	}) {
		return base + "; Cilium detected — source preservation requires a " +
			"masquerade exclusion (BPF ip-masq-agent nonMasqueradeCIDRs) for " +
			"prefixes accepted from remote ASes (see docs/install.md, Vanilla Kubernetes)"
	}
	return base + "; ensure the CNI excludes prefixes accepted from remote ASes " +
		"from pod-egress masquerade (see docs/install.md, Vanilla Kubernetes)"
}

// apiPresent probes for an optional API by listing it with a limit of 1.
func (r *ScionNetworkReconciler) apiPresent(ctx context.Context, listGVK schema.GroupVersionKind) bool {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(listGVK)
	return r.List(ctx, list, client.Limit(1)) == nil
}

func classifyPodEgressPlatform(network *unstructured.Unstructured) podEgressPlatform {
	networkType, _, _ := unstructured.NestedString(network.Object, "spec", "defaultNetwork", "type")
	if networkType != "OVNKubernetes" {
		return podEgressPlatform{
			Status:  metav1.ConditionUnknown,
			Reason:  "PlatformUnverified",
			Message: fmt.Sprintf("default network type %q is not OVN-Kubernetes", networkType),
		}
	}
	routingViaHost, found, err := unstructured.NestedBool(network.Object,
		"spec", "defaultNetwork", "ovnKubernetesConfig", "gatewayConfig", "routingViaHost")
	if err != nil {
		return podEgressPlatform{
			Status:  metav1.ConditionFalse,
			Reason:  "HostRoutingDisabled",
			Message: "OVN-Kubernetes routingViaHost has an invalid value",
		}
	}
	if !found || !routingViaHost {
		return podEgressPlatform{
			Status: metav1.ConditionFalse,
			Reason: "HostRoutingDisabled",
			Message: "OVN-Kubernetes shared-gateway mode bypasses host routes; " +
				"set spec.defaultNetwork.ovnKubernetesConfig.gatewayConfig.routingViaHost=true",
		}
	}
	return podEgressPlatform{
		Status:  metav1.ConditionTrue,
		Reason:  "HostRoutingEnabled",
		Message: "OVN-Kubernetes routes pod egress through the node host routing table",
	}
}

func sourcePreservationDisabled(message string) podEgressPlatform {
	return podEgressPlatform{
		Status:  metav1.ConditionFalse,
		Reason:  "SourcePreservationDisabled",
		Message: message,
	}
}

func hasAcceptedDefaultPodAdvertisement(items []unstructured.Unstructured) bool {
	for i := range items {
		item := &items[i]
		if !routeAdvertisementAccepted(item) {
			continue
		}
		advertisements, _, _ := unstructured.NestedStringSlice(item.Object, "spec", "advertisements")
		if !slices.Contains(advertisements, "PodNetwork") {
			continue
		}
		selectors, _, _ := unstructured.NestedSlice(item.Object, "spec", "networkSelectors")
		for _, selector := range selectors {
			fields, ok := selector.(map[string]interface{})
			if ok && fields["networkSelectionType"] == "DefaultNetwork" {
				return true
			}
		}
	}
	return false
}

// routeAdvertisementAccepted reports whether a RouteAdvertisements object is
// accepted. OVN-Kubernetes currently publishes a bare status.status string
// ("Accepted"); a conditions-based form (type=Accepted, status=True) is also
// recognized so a CRD status evolution does not silently disable detection.
func routeAdvertisementAccepted(item *unstructured.Unstructured) bool {
	if status, _, _ := unstructured.NestedString(item.Object, "status", "status"); status == "Accepted" {
		return true
	}
	conditions, _, _ := unstructured.NestedSlice(item.Object, "status", "conditions")
	for _, condition := range conditions {
		fields, ok := condition.(map[string]interface{})
		if ok && fields["type"] == "Accepted" && fields["status"] == "True" {
			return true
		}
	}
	return false
}
