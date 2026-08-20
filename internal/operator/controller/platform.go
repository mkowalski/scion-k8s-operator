package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type podEgressPlatform struct {
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

func (r *ScionNetworkReconciler) detectPodEgressPlatform(ctx context.Context) (podEgressPlatform, error) {
	network := &unstructured.Unstructured{}
	network.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "operator.openshift.io", Version: "v1", Kind: "Network",
	})
	if err := r.Get(ctx, types.NamespacedName{Name: "cluster"}, network); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return podEgressPlatform{
				Status:  metav1.ConditionUnknown,
				Reason:  "PlatformUnverified",
				Message: "OpenShift OVN-Kubernetes gateway configuration is not available",
			}, nil
		}
		return podEgressPlatform{}, fmt.Errorf("read OpenShift network operator config: %w", err)
	}
	platform := classifyPodEgressPlatform(network)
	if platform.Status != metav1.ConditionTrue {
		return platform, nil
	}
	routeAdvertisements, _, _ := unstructured.NestedString(network.Object,
		"spec", "defaultNetwork", "ovnKubernetesConfig", "routeAdvertisements")
	if routeAdvertisements != "Enabled" {
		return sourcePreservationDisabled("OVN routeAdvertisements must be Enabled to preserve pod source addresses"), nil
	}

	routes := &unstructured.UnstructuredList{}
	routes.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.ovn.org", Version: "v1", Kind: "RouteAdvertisementsList",
	})
	if err := r.List(ctx, routes); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return sourcePreservationDisabled("OVN RouteAdvertisements API is not available"), nil
		}
		return podEgressPlatform{}, fmt.Errorf("list OVN RouteAdvertisements: %w", err)
	}
	if !hasAcceptedDefaultPodAdvertisement(routes.Items) {
		return sourcePreservationDisabled("no accepted RouteAdvertisements exports PodNetwork for the default network"), nil
	}
	return podEgressPlatform{
		Status:  metav1.ConditionTrue,
		Reason:  "HostRoutingEnabled",
		Message: "OVN-Kubernetes routes pod egress through the host with pod source preservation",
	}, nil
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
		status, _, _ := unstructured.NestedString(item.Object, "status", "status")
		if status != "Accepted" {
			continue
		}
		advertisements, _, _ := unstructured.NestedStringSlice(item.Object, "spec", "advertisements")
		containsPodNetwork := false
		for _, advertisement := range advertisements {
			if advertisement == "PodNetwork" {
				containsPodNetwork = true
				break
			}
		}
		if !containsPodNetwork {
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
