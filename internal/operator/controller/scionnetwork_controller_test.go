package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
	// The namespace must NOT be owned by the ScionNetwork: the operator
	// Deployment lives in scion-system, so GC of the namespace on CR
	// deletion would kill the operator. Lifecycle belongs to the deploy
	// manifests.
	if len(ns.OwnerReferences) != 0 {
		t.Fatalf("namespace must not have owner references: %+v", ns.OwnerReferences)
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

// TestRegistrarDesiredSIGsPublished creates a labeled node with an
// InternalIP and asserts the default (manual) backend publishes the
// desired SIG set in status.registrar.
func TestRegistrarDesiredSIGsPublished(t *testing.T) {
	skipIfNoEnvtest(t)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node1",
			Labels: map[string]string{"role": "edge"},
		},
	}
	if err := k8sClient.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete node: %v", err)
		}
	})
	node.Status.Addresses = []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "192.0.2.10"},
	}
	if err := k8sClient.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}

	sn := newScionNetwork()
	sn.Spec.NodeSelector = map[string]string{"role": "edge"}
	if err := k8sClient.Create(ctx, sn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, sn) })

	want := "node1=192.0.2.10:30256,192.0.2.10:30056"
	eventually(t, func() bool {
		got := &v1alpha1.ScionNetwork{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "cluster"}, got); err != nil {
			return false
		}
		for _, s := range got.Status.Registrar.DesiredSIGs {
			if s == want {
				return got.Status.Registrar.RegisteredNodes == 1 &&
					got.Status.Registrar.LastError == "" &&
					got.Status.Registrar.LastSyncTime != nil
			}
		}
		return false
	}, "status.registrar.desiredSIGs missing "+want)
}

// TestStatusISDASFromDiscoveryURL points spec.bootstrap.discoveryURL at an
// in-process httptest server (reachable because the reconciler runs in this
// process) and asserts the discovered isd_as lands in status.isdAS.
func TestStatusISDASFromDiscoveryURL(t *testing.T) {
	skipIfNoEnvtest(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/topology" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"isd_as": "1-ff00:0:112", "mtu": 1472}`))
	}))
	defer srv.Close()

	sn := newScionNetwork()
	sn.Spec.Bootstrap.DiscoveryURL = srv.URL
	if err := k8sClient.Create(ctx, sn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, sn) })

	eventually(t, func() bool {
		got := &v1alpha1.ScionNetwork{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "cluster"}, got); err != nil {
			return false
		}
		return got.Status.ISDAS == "1-ff00:0:112"
	}, "status.isdAS not set to 1-ff00:0:112")
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

// TestApplyFailureSurfacesDegraded uses the fake client (no envtest) with an
// interceptor that fails DaemonSet creation, asserting the reconciler
// records Degraded=True/ApplyFailed in status before returning the error.
func TestApplyFailureSurfacesDegraded(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	sn := newScionNetwork()
	boom := errors.New("injected daemonset failure")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sn).
		WithStatusSubresource(&v1alpha1.ScionNetwork{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*appsv1.DaemonSet); ok {
					return boom
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()
	r := &ScionNetworkReconciler{Client: c, Scheme: scheme, AgentImage: testAgentImage}

	_, err := r.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if err == nil || !strings.Contains(err.Error(), "injected daemonset failure") {
		t.Fatalf("Reconcile error = %v, want injected failure", err)
	}

	got := &v1alpha1.ScionNetwork{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, got); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, "Degraded")
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "ApplyFailed" {
		t.Fatalf("Degraded condition = %+v, want True/ApplyFailed", cond)
	}
	if !strings.Contains(cond.Message, "injected daemonset failure") {
		t.Fatalf("Degraded message = %q, want to contain the error", cond.Message)
	}
}
