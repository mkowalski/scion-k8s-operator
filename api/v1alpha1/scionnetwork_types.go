package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BootstrapSpec configures how node agents discover the SCION AS.
// +kubebuilder:validation:XValidation:rule="self.mode != 'url' || (has(self.discoveryURL) && size(self.discoveryURL) > 0)",message="discoveryURL required when mode is url"
type BootstrapSpec struct {
	// Mode selects the bootstrap discovery mechanism.
	// +kubebuilder:validation:Enum=url;dns;dhcp;mdns
	Mode string `json:"mode"`
	// DiscoveryURL is the bootstrap server URL; required when mode is url.
	// +optional
	DiscoveryURL string `json:"discoveryURL,omitempty"`
	// DNSDomain is the search domain used for DNS-based discovery.
	// +optional
	DNSDomain string `json:"dnsDomain,omitempty"`
	// DHCPInterface is the network interface used for DHCP-based discovery.
	// +optional
	DHCPInterface string `json:"dhcpInterface,omitempty"`
	// SecretRef optionally names a Secret mounted as pinned TRCs; keys
	// must be TRC filenames (e.g. ISD1-B1-S1.trc). Authenticated bootstrap
	// is not yet implemented.
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`
	// RefreshInterval is how often the bootstrap data is refreshed.
	// +optional
	// +kubebuilder:default:="1h"
	// +kubebuilder:validation:Pattern=`^[0-9]+(ns|us|µs|ms|s|m|h)([0-9]+(ns|us|µs|ms|s|m|h))*$`
	RefreshInterval string `json:"refreshInterval,omitempty"`
}

// AdvertisementSpec controls which local prefixes are advertised to remotes.
type AdvertisementSpec struct {
	// PodCIDR enables advertising each node's pod CIDR.
	// +kubebuilder:default:=true
	PodCIDR *bool `json:"podCIDR,omitempty"`
	// NodeIP enables advertising each node's IP address. Disabled by
	// default: when node IPs share the network that carries the SCION
	// tunnel traffic (the underlay), advertising them makes remote ASes
	// route the underlay into the tunnel, causing a routing loop. Enable
	// only when node IPs are disjoint from the underlay or the remote
	// side excludes it (see spec.acceptPolicy.underlayCIDRs).
	// +kubebuilder:default:=false
	NodeIP *bool `json:"nodeIP,omitempty"`
}

// AcceptPolicySpec controls which remote prefixes are accepted.
type AcceptPolicySpec struct {
	// ISDASes lists remote ISD-ASes to exchange prefixes with.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Pattern=`^\d+-([0-9a-fA-F_:]+|\d+)$`
	ISDASes []string `json:"isdASes"`
	// ForbiddenCIDRs are never accepted from remotes; the operator appends
	// clusterNetwork/serviceNetwork IPv4 ranges automatically. IPv6 ranges
	// are ignored because the policy engine is IPv4-only.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=18
	// +kubebuilder:validation:items:Pattern=`^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$`
	// +kubebuilder:validation:items:XValidation:rule="isCIDR(self)",message="must be a valid IPv4 CIDR"
	ForbiddenCIDRs []string `json:"forbiddenCIDRs,omitempty"`
	// UnderlayCIDRs are never accepted from remotes and keep cluster-to-AS
	// transport reachable outside the SCION tunnel.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=18
	// +kubebuilder:validation:items:Pattern=`^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$`
	// +kubebuilder:validation:items:XValidation:rule="isCIDR(self)",message="must be a valid IPv4 CIDR"
	UnderlayCIDRs []string `json:"underlayCIDRs,omitempty"`
}

// DataplaneSpec configures the node-local SCION dataplane.
type DataplaneSpec struct {
	// TunName is the name of the TUN device created on each node.
	// +kubebuilder:default:="scion0"
	TunName string `json:"tunName,omitempty"`
}

// RegistrarSpec configures how SIG endpoints are registered with the AS.
// +kubebuilder:validation:XValidation:rule="self.backend == 'manual' || (has(self.endpoint) && size(self.endpoint) > 0)",message="endpoint required when backend is not manual"
type RegistrarSpec struct {
	// Backend selects the registration mechanism.
	// +kubebuilder:validation:Enum=manual;http;anapaya
	// +kubebuilder:default:="manual"
	Backend string `json:"backend,omitempty"`
	// Endpoint is the registration API endpoint for http/anapaya backends.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// CredentialsSecretRef holds credentials for the registration backend.
	// +optional
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`
}

// ScionNetworkSpec defines the desired state of ScionNetwork.
type ScionNetworkSpec struct {
	// Bootstrap configures SCION AS discovery for node agents.
	Bootstrap BootstrapSpec `json:"bootstrap"`
	// Advertisement controls which local prefixes are advertised.
	// +optional
	Advertisement AdvertisementSpec `json:"advertisement,omitempty"`
	// AcceptPolicy controls which remote prefixes are accepted.
	AcceptPolicy AcceptPolicySpec `json:"acceptPolicy"`
	// Dataplane configures the node-local SCION dataplane.
	// +optional
	Dataplane DataplaneSpec `json:"dataplane,omitempty"`
	// Registrar configures SIG endpoint registration.
	// +optional
	Registrar RegistrarSpec `json:"registrar,omitempty"`
	// NodeSelector limits which nodes run the agent DaemonSet.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Tolerations are applied to the agent DaemonSet pods.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// AgentImage overrides the default agent image (set by operator build).
	// +optional
	AgentImage string `json:"agentImage,omitempty"`
}

// NodeSummary summarizes agent readiness across nodes.
type NodeSummary struct {
	// Ready is the number of nodes with a ready agent.
	Ready int32 `json:"ready"`
	// Total is the number of nodes that should run an agent.
	Total int32 `json:"total"`
	// Degraded lists nodes whose agent is not ready.
	// +optional
	Degraded []string `json:"degraded,omitempty"`
}

// RegistrarStatus reports the state of SIG endpoint registration.
type RegistrarStatus struct {
	// RegisteredNodes is the number of nodes successfully registered.
	// +optional
	RegisteredNodes int32 `json:"registeredNodes,omitempty"`
	// DesiredSIGs is published for backend=manual so AS operators can copy it.
	// +optional
	DesiredSIGs []string `json:"desiredSIGs,omitempty"`
	// LastSyncTime is the last time registration was synced.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
	// LastError is the most recent registration error, if any.
	// +optional
	LastError string `json:"lastError,omitempty"`
}

// ScionNetworkStatus defines the observed state of ScionNetwork.
type ScionNetworkStatus struct {
	// Conditions represent the current state of the ScionNetwork.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ISDAS is the local ISD-AS discovered via bootstrap.
	// +optional
	ISDAS string `json:"isdAS,omitempty"`
	// Nodes summarizes agent readiness across nodes.
	// +optional
	Nodes NodeSummary `json:"nodes,omitempty"`
	// Registrar reports SIG registration state.
	// +optional
	Registrar RegistrarStatus `json:"registrar,omitempty"`
}

// ScionNetwork is the cluster-scoped singleton configuring SCION connectivity.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="ScionNetwork is a singleton; metadata.name must be 'cluster'"
// +kubebuilder:printcolumn:name="ISD-AS",type=string,JSONPath=`.status.isdAS`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.nodes.ready`
type ScionNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the desired state.
	Spec ScionNetworkSpec `json:"spec,omitempty"`
	// Status is the observed state.
	Status ScionNetworkStatus `json:"status,omitempty"`
}

// ScionNetworkList contains a list of ScionNetwork.
// +kubebuilder:object:root=true
type ScionNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ScionNetwork `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ScionNetwork{}, &ScionNetworkList{})
}
