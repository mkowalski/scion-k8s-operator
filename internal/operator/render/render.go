// Package render builds the Kubernetes objects owned by the operator for a
// ScionNetwork. All builders are pure functions: input spec, output objects;
// no API calls.
package render

import (
	"maps"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"

	v1alpha1 "github.com/mkowalski/scion-k8s-operator/api/v1alpha1"
)

const (
	// Namespace is where all namespaced operator-owned objects live.
	Namespace = "scion-system"
	agentName = "scion-node-agent"
	// stateDir is the hostPath cache used by the agent (SCION_STATE_DIR
	// default in internal/agent/config).
	stateDir = "/var/lib/scion-node-agent"
	// pinnedTRCsDir is where bootstrap.Fetch reads pinned TRCs from:
	// confDir is <stateDir>/etc and pins are read from
	// <confDir>/../pinned-trcs, i.e. <stateDir>/pinned-trcs. Secret keys
	// must be named after the TRC file, e.g. "ISD1-B1-S1.trc".
	pinnedTRCsDir = stateDir + "/pinned-trcs"
	metricsPort   = 9465
)

func labels() map[string]string {
	return map[string]string{"app": agentName}
}

// boolOr dereferences b, returning def when nil.
func boolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

func boolStr(b bool) string {
	return strconv.FormatBool(b)
}

// probe builds an HTTP probe against the agent metrics/health port.
func probe(path string) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: path,
				Port: intstr.FromInt32(metricsPort),
			},
		},
	}
}

// DaemonSet renders the node-agent DaemonSet for the given ScionNetwork.
// forbiddenCIDRs is the merged list (spec + cluster/service networks)
// computed by the controller.
func DaemonSet(sn *v1alpha1.ScionNetwork, image string, forbiddenCIDRs []string) *appsv1.DaemonSet {
	hostPathChar := corev1.HostPathCharDev
	hostPathDir := corev1.HostPathDirectoryOrCreate
	priv := false
	// Intentionally unset (agent defaults are correct as-is):
	// SCION_STATE_DIR (/var/lib/scion-node-agent), SCION_METRICS_ADDR
	// (:9465), SCION_ENABLE_DAEMON_API (true).
	env := []corev1.EnvVar{
		{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
		{Name: "SCION_BOOTSTRAP_MODE", Value: sn.Spec.Bootstrap.Mode},
		{Name: "SCION_DISCOVERY_URL", Value: sn.Spec.Bootstrap.DiscoveryURL},
		{Name: "SCION_DNS_DOMAIN", Value: sn.Spec.Bootstrap.DNSDomain},
		{Name: "SCION_DHCP_INTERFACE", Value: sn.Spec.Bootstrap.DHCPInterface},
		{Name: "SCION_REFRESH_INTERVAL", Value: sn.Spec.Bootstrap.RefreshInterval},
		{Name: "SCION_TUN_NAME", Value: sn.Spec.Dataplane.TunName},
		{Name: "SCION_ACCEPT_ISD_ASES", Value: strings.Join(sn.Spec.AcceptPolicy.ISDASes, ",")},
		{Name: "SCION_FORBIDDEN_CIDRS", Value: strings.Join(forbiddenCIDRs, ",")},
		{Name: "SCION_ADVERTISE_POD_CIDR", Value: boolStr(boolOr(sn.Spec.Advertisement.PodCIDR, true))},
		{Name: "SCION_ADVERTISE_NODE_IP", Value: boolStr(boolOr(sn.Spec.Advertisement.NodeIP, true))},
	}
	volumes := []corev1.Volume{
		{Name: "state", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
			Path: stateDir, Type: &hostPathDir}}},
		{Name: "tun", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
			Path: "/dev/net/tun", Type: &hostPathChar}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: "state", MountPath: stateDir},
		{Name: "tun", MountPath: "/dev/net/tun"},
	}
	if ref := sn.Spec.Bootstrap.SecretRef; ref != nil {
		volumes = append(volumes, corev1.Volume{
			Name: "pinned-trcs",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: ref.Name}},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name: "pinned-trcs", MountPath: pinnedTRCsDir, ReadOnly: true})
	}
	l := labels()
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: Namespace, Labels: l},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: l},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: l},
				Spec: corev1.PodSpec{
					ServiceAccountName: agentName,
					HostNetwork:        true,
					PriorityClassName:  "system-node-critical",
					NodeSelector:       maps.Clone(sn.Spec.NodeSelector),
					Tolerations: append(append([]corev1.Toleration{}, sn.Spec.Tolerations...),
						corev1.Toleration{Operator: corev1.TolerationOpExists}),
					Containers: []corev1.Container{{
						Name:  "agent",
						Image: image,
						Env:   env,
						SecurityContext: &corev1.SecurityContext{
							Privileged: &priv,
							Capabilities: &corev1.Capabilities{
								Add: []corev1.Capability{"NET_ADMIN"},
							},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
						},
						VolumeMounts:   mounts,
						ReadinessProbe: probe("/readyz"),
						LivenessProbe:  probe("/healthz"),
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// ServiceAccount renders the agent ServiceAccount.
func ServiceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: Namespace, Labels: labels()},
	}
}

// ClusterRole renders the agent ClusterRole; the agent only needs to read
// its own Node object (pod CIDR, addresses).
func ClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Labels: labels()},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"nodes"},
			Verbs:     []string{"get"},
		}},
	}
}

// ClusterRoleBinding binds the agent ClusterRole to its ServiceAccount.
func ClusterRoleBinding() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Labels: labels()},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     agentName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      agentName,
			Namespace: Namespace,
		}},
	}
}

// SCC renders the OpenShift SecurityContextConstraints as unstructured so
// vanilla Kubernetes builds do not import OpenShift API types. The
// controller only applies it when the SCC API is available.
func SCC() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "security.openshift.io/v1",
		"kind":       "SecurityContextConstraints",
		"metadata": map[string]interface{}{
			"name":   agentName,
			"labels": map[string]interface{}{"app": agentName},
		},
		"allowHostDirVolumePlugin": true,
		"allowHostIPC":             false,
		"allowHostNetwork":         true,
		"allowHostPID":             false,
		"allowHostPorts":           true,
		"allowPrivilegeEscalation": false,
		"allowPrivilegedContainer": false,
		"allowedCapabilities":      []interface{}{"NET_ADMIN"},
		"fsGroup":                  map[string]interface{}{"type": "RunAsAny"},
		"readOnlyRootFilesystem":   false,
		"runAsUser":                map[string]interface{}{"type": "RunAsAny"},
		"seLinuxContext":           map[string]interface{}{"type": "RunAsAny"},
		"supplementalGroups":       map[string]interface{}{"type": "RunAsAny"},
		"volumes": []interface{}{
			"hostPath", "secret", "downwardAPI", "configMap", "projected", "emptyDir",
		},
		"users":  []interface{}{"system:serviceaccount:" + Namespace + ":" + agentName},
		"groups": []interface{}{},
	}}
}

// NamespaceObj renders the scion-system Namespace with pod-security labels
// permitting the hostNetwork agent pods (privileged PSA level).
func NamespaceObj() *corev1.Namespace {
	l := labels()
	for _, k := range []string{"enforce", "audit", "warn"} {
		l["pod-security.kubernetes.io/"+k] = "privileged"
	}
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: Namespace, Labels: l},
	}
}
