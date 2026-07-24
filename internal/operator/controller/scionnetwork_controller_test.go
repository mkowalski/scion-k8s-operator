package controller

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mkowalski/scion-k8s-operator/api/v1alpha1"
	"github.com/mkowalski/scion-k8s-operator/internal/operator/render"
)

func newScionNetwork() *v1alpha1.ScionNetwork {
	sn := &v1alpha1.ScionNetwork{}
	sn.Name = "cluster"
	sn.Spec.Bootstrap.Mode = "url"
	sn.Spec.Bootstrap.DiscoveryURL = "http://ds:8041"
	sn.Spec.AcceptPolicy.ISDASes = []string{"1-ff00:0:110"}
	sn.Spec.AcceptPolicy.ForbiddenCIDRs = []string{"192.0.2.0/24"}
	return sn
}

func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", msg)
}

func dsKey() types.NamespacedName {
	return types.NamespacedName{Namespace: render.Namespace, Name: "scion-node-agent"}
}

func TestReconcileCreatesDaemonSet(t *testing.T) {
	skipIfNoEnvtest(t)

	sn := newScionNetwork()
	if err := k8sClient.Create(ctx, sn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, sn) })

	ds := &appsv1.DaemonSet{}
	eventually(t, func() bool {
		return k8sClient.Get(ctx, dsKey(), ds) == nil
	}, "DaemonSet scion-node-agent not created")

	// Forbidden CIDRs from spec must be in the agent env.
	var forbidden string
	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "SCION_FORBIDDEN_CIDRS" {
			forbidden = e.Value
		}
	}
	if !strings.Contains(forbidden, "192.0.2.0/24") {
		t.Fatalf("SCION_FORBIDDEN_CIDRS = %q, want to contain 192.0.2.0/24", forbidden)
	}
	if got := ds.Spec.Template.Spec.Containers[0].Image; got != testAgentImage {
		t.Fatalf("image = %q, want %q", got, testAgentImage)
	}

	// Owner references: ScionNetwork owns the DaemonSet (cluster-scoped
	// owner on a namespaced dependent is allowed by k8s GC).
	if len(ds.OwnerReferences) != 1 || ds.OwnerReferences[0].Kind != "ScionNetwork" {
		t.Fatalf("ownerReferences: %+v", ds.OwnerReferences)
	}

	// Supporting objects.
	ns := &corev1.Namespace{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: render.Namespace}, ns); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	if ns.Labels["pod-security.kubernetes.io/enforce"] != "privileged" {
		t.Fatalf("namespace PSA labels: %v", ns.Labels)
	}
	for _, obj := range []client.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: render.Namespace, Name: "scion-node-agent"}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "scion-node-agent"}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "scion-node-agent"}},
	} {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatalf("get %T: %v", obj, err)
		}
	}

	// Status conditions: no agent pods run in envtest, so Progressing=True
	// and Available=False.
	eventually(t, func() bool {
		got := &v1alpha1.ScionNetwork{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "cluster"}, got); err != nil {
			return false
		}
		var progressing, available *metav1.Condition
		for i := range got.Status.Conditions {
			switch got.Status.Conditions[i].Type {
			case "Progressing":
				progressing = &got.Status.Conditions[i]
			case "Available":
				available = &got.Status.Conditions[i]
			}
		}
		return progressing != nil && progressing.Status == metav1.ConditionTrue &&
			available != nil && available.Status == metav1.ConditionFalse
	}, "status conditions not set (Progressing=True, Available=False)")
}

func TestReconcileRepairsDeletedDaemonSet(t *testing.T) {
	skipIfNoEnvtest(t)

	sn := newScionNetwork()
	if err := k8sClient.Create(ctx, sn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, sn) })

	ds := &appsv1.DaemonSet{}
	eventually(t, func() bool {
		return k8sClient.Get(ctx, dsKey(), ds) == nil
	}, "DaemonSet not created")

	uid := ds.UID
	if err := k8sClient.Delete(ctx, ds); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		got := &appsv1.DaemonSet{}
		return k8sClient.Get(ctx, dsKey(), got) == nil && got.UID != uid
	}, "DaemonSet not recreated after delete")
}
