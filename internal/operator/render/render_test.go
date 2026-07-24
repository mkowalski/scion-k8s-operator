package render

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/mkowalski/scion-k8s-operator/api/v1alpha1"
)

func testSN() *v1alpha1.ScionNetwork {
	sn := &v1alpha1.ScionNetwork{}
	sn.Name = "cluster"
	sn.Spec.Bootstrap.Mode = "url"
	sn.Spec.Bootstrap.DiscoveryURL = "http://ds:8041"
	sn.Spec.AcceptPolicy.ISDASes = []string{"1-ff00:0:110"}
	return sn
}

func TestDaemonSet(t *testing.T) {
	sn := testSN()
	ds := DaemonSet(sn, "quay.io/mkowalski/scion-node-agent:0.1.0", []string{"10.128.0.0/14", "172.30.0.0/16"})
	if !ds.Spec.Template.Spec.HostNetwork {
		t.Fatal("agent must be hostNetwork")
	}
	env := map[string]string{}
	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["SCION_DISCOVERY_URL"] != "http://ds:8041" {
		t.Fatalf("env: %v", env)
	}
	if env["SCION_FORBIDDEN_CIDRS"] != "10.128.0.0/14,172.30.0.0/16" {
		t.Fatalf("forbidden cidrs env: %v", env)
	}
	if env["SCION_BOOTSTRAP_MODE"] != "url" {
		t.Fatalf("bootstrap mode env: %v", env)
	}
	if env["SCION_ACCEPT_ISD_ASES"] != "1-ff00:0:110" {
		t.Fatalf("isd-ases env: %v", env)
	}
}

func TestDaemonSetBoolEnvDefaults(t *testing.T) {
	sn := testSN()
	// nil *bool -> "true"
	ds := DaemonSet(sn, "img", nil)
	env := map[string]string{}
	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["SCION_ADVERTISE_POD_CIDR"] != "true" || env["SCION_ADVERTISE_NODE_IP"] != "true" {
		t.Fatalf("bool defaults: %v", env)
	}
	f := false
	sn.Spec.Advertisement.PodCIDR = &f
	ds = DaemonSet(sn, "img", nil)
	env = map[string]string{}
	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["SCION_ADVERTISE_POD_CIDR"] != "false" {
		t.Fatalf("explicit false: %v", env)
	}
}

func TestDaemonSetNodeNameDownwardAPI(t *testing.T) {
	ds := DaemonSet(testSN(), "img", nil)
	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "NODE_NAME" {
			if e.ValueFrom == nil || e.ValueFrom.FieldRef == nil || e.ValueFrom.FieldRef.FieldPath != "spec.nodeName" {
				t.Fatalf("NODE_NAME must come from downward API spec.nodeName: %+v", e)
			}
			return
		}
	}
	t.Fatal("NODE_NAME env missing")
}

func TestDaemonSetSchedulingAndTolerations(t *testing.T) {
	sn := testSN()
	sn.Spec.NodeSelector = map[string]string{"kubernetes.io/os": "linux"}
	sn.Spec.Tolerations = []corev1.Toleration{{Key: "custom", Operator: corev1.TolerationOpEqual, Value: "x"}}
	ds := DaemonSet(sn, "img", nil)
	ps := ds.Spec.Template.Spec
	if ps.NodeSelector["kubernetes.io/os"] != "linux" {
		t.Fatalf("nodeSelector not passed through: %v", ps.NodeSelector)
	}
	if len(ps.Tolerations) != 2 {
		t.Fatalf("expected spec toleration + Exists toleration, got %v", ps.Tolerations)
	}
	if ps.Tolerations[0].Key != "custom" {
		t.Fatalf("spec toleration missing: %v", ps.Tolerations)
	}
	last := ps.Tolerations[1]
	if last.Operator != corev1.TolerationOpExists || last.Key != "" {
		t.Fatalf("expected blanket Exists toleration, got %+v", last)
	}
	if ps.PriorityClassName != "system-node-critical" {
		t.Fatalf("priorityClassName: %q", ps.PriorityClassName)
	}
	if ps.ServiceAccountName != "scion-node-agent" {
		t.Fatalf("serviceAccountName: %q", ps.ServiceAccountName)
	}
}

func TestDaemonSetProbesAndSecurity(t *testing.T) {
	ds := DaemonSet(testSN(), "img", nil)
	c := ds.Spec.Template.Spec.Containers[0]
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil ||
		c.ReadinessProbe.HTTPGet.Path != "/readyz" || c.ReadinessProbe.HTTPGet.Port.IntValue() != 9465 {
		t.Fatalf("readiness probe: %+v", c.ReadinessProbe)
	}
	if c.LivenessProbe == nil || c.LivenessProbe.HTTPGet == nil ||
		c.LivenessProbe.HTTPGet.Path != "/healthz" || c.LivenessProbe.HTTPGet.Port.IntValue() != 9465 {
		t.Fatalf("liveness probe: %+v", c.LivenessProbe)
	}
	sc := c.SecurityContext
	// TODO(live-e2e): privileged root is required on RHCOS; see render.go.
	if sc == nil || sc.Privileged == nil || !*sc.Privileged {
		t.Fatalf("must be privileged (RHCOS live-e2e requirement): %+v", sc)
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 0 {
		t.Fatalf("must run as root: %+v", sc)
	}
	if c.Resources.Requests.Cpu().String() != "50m" || c.Resources.Requests.Memory().String() != "64Mi" {
		t.Fatalf("resource requests: %+v", c.Resources)
	}
	if len(c.Resources.Limits) != 0 {
		t.Fatalf("no limits expected: %+v", c.Resources.Limits)
	}
}

func TestDaemonSetVolumes(t *testing.T) {
	ds := DaemonSet(testSN(), "img", nil)
	vols := map[string]corev1.Volume{}
	for _, v := range ds.Spec.Template.Spec.Volumes {
		vols[v.Name] = v
	}
	st, ok := vols["state"]
	if !ok || st.HostPath == nil || st.HostPath.Path != "/var/lib/scion-node-agent" {
		t.Fatalf("state volume: %+v", st)
	}
	tun, ok := vols["tun"]
	if !ok || tun.HostPath == nil || tun.HostPath.Path != "/dev/net/tun" {
		t.Fatalf("tun volume: %+v", tun)
	}
	mounts := map[string]string{}
	for _, m := range ds.Spec.Template.Spec.Containers[0].VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	if mounts["state"] != "/var/lib/scion-node-agent" || mounts["tun"] != "/dev/net/tun" {
		t.Fatalf("volume mounts: %v", mounts)
	}
	if _, ok := vols["pinned-trcs"]; ok {
		t.Fatal("pinned-trcs volume must not exist without secretRef")
	}
}

func TestDaemonSetPinnedTRCSecret(t *testing.T) {
	sn := testSN()
	sn.Spec.Bootstrap.SecretRef = &corev1.LocalObjectReference{Name: "trc-pins"}
	ds := DaemonSet(sn, "img", nil)
	var vol *corev1.Volume
	for i, v := range ds.Spec.Template.Spec.Volumes {
		if v.Name == "pinned-trcs" {
			vol = &ds.Spec.Template.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.Secret == nil || vol.Secret.SecretName != "trc-pins" {
		t.Fatalf("pinned-trcs secret volume: %+v", vol)
	}
	found := false
	for _, m := range ds.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == "pinned-trcs" {
			found = true
			// bootstrap.Fetch reads pins from <StateDir>/pinned-trcs
			// (confDir=<StateDir>/etc, pin path <confDir>/../pinned-trcs).
			if m.MountPath != "/var/lib/scion-node-agent/pinned-trcs" {
				t.Fatalf("pinned-trcs mount path: %q", m.MountPath)
			}
			if !m.ReadOnly {
				t.Fatalf("pinned-trcs mount must be read-only")
			}
		}
	}
	if !found {
		t.Fatal("pinned-trcs mount missing")
	}
}

func TestServiceAccount(t *testing.T) {
	sa := ServiceAccount()
	if sa.Name != "scion-node-agent" || sa.Namespace != "scion-system" {
		t.Fatalf("serviceaccount: %+v", sa.ObjectMeta)
	}
}

func TestClusterRole(t *testing.T) {
	cr := ClusterRole()
	if cr.Name != "scion-node-agent" {
		t.Fatalf("name: %q", cr.Name)
	}
	if len(cr.Rules) != 1 {
		t.Fatalf("rules: %+v", cr.Rules)
	}
	r := cr.Rules[0]
	if len(r.APIGroups) != 1 || r.APIGroups[0] != "" ||
		len(r.Resources) != 1 || r.Resources[0] != "nodes" ||
		len(r.Verbs) != 1 || r.Verbs[0] != "get" {
		t.Fatalf("rule shape: %+v", r)
	}
}

func TestClusterRoleBinding(t *testing.T) {
	crb := ClusterRoleBinding()
	if crb.RoleRef.Kind != "ClusterRole" || crb.RoleRef.Name != "scion-node-agent" {
		t.Fatalf("roleRef: %+v", crb.RoleRef)
	}
	if len(crb.Subjects) != 1 || crb.Subjects[0].Kind != "ServiceAccount" ||
		crb.Subjects[0].Name != "scion-node-agent" || crb.Subjects[0].Namespace != "scion-system" {
		t.Fatalf("subjects: %+v", crb.Subjects)
	}
}

func TestSCC(t *testing.T) {
	scc := SCC()
	if scc.GetAPIVersion() != "security.openshift.io/v1" || scc.GetKind() != "SecurityContextConstraints" {
		t.Fatalf("gvk: %s %s", scc.GetAPIVersion(), scc.GetKind())
	}
	if scc.GetName() != "scion-node-agent" {
		t.Fatalf("name: %q", scc.GetName())
	}
	obj := scc.Object
	if obj["allowHostNetwork"] != true || obj["allowHostDirVolumePlugin"] != true {
		t.Fatalf("host flags: %v", obj)
	}
	if obj["allowPrivilegedContainer"] != true {
		t.Fatalf("must allow privileged (RHCOS live-e2e requirement): %v", obj)
	}
	caps, _ := obj["allowedCapabilities"].([]interface{})
	if len(caps) != 0 {
		t.Fatalf("allowedCapabilities must be empty (moot under privileged): %v", obj["allowedCapabilities"])
	}
	users, _ := obj["users"].([]interface{})
	if len(users) != 1 || users[0] != "system:serviceaccount:scion-system:scion-node-agent" {
		t.Fatalf("users: %v", obj["users"])
	}
}

func TestNamespaceObj(t *testing.T) {
	ns := NamespaceObj()
	if ns.Name != "scion-system" {
		t.Fatalf("name: %q", ns.Name)
	}
	for _, k := range []string{
		"pod-security.kubernetes.io/enforce",
		"pod-security.kubernetes.io/audit",
		"pod-security.kubernetes.io/warn",
	} {
		if ns.Labels[k] != "privileged" {
			t.Fatalf("label %s: %q", k, ns.Labels[k])
		}
	}
}
