package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
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

// deleteAndWait deletes the ScionNetwork and blocks until it is actually
// gone. Deletion is asynchronous since the registrar-cleanup finalizer was
// added: Delete only sets deletionTimestamp and the running reconciler
// removes the finalizer afterwards. Every envtest test creating the
// "cluster" singleton must use this in t.Cleanup, or the next test's
// Create fails with "object is being deleted".
func deleteAndWait(t *testing.T, sn *v1alpha1.ScionNetwork) {
	t.Helper()
	if err := k8sClient.Delete(ctx, sn); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("delete ScionNetwork: %v", err)
		return
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: sn.Name}, &v1alpha1.ScionNetwork{})
		if apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("ScionNetwork %q still present after delete (finalizer stuck?)", sn.Name)
}

func TestReconcileCreatesDaemonSet(t *testing.T) {
	skipIfNoEnvtest(t)

	sn := newScionNetwork()
	if err := k8sClient.Create(ctx, sn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteAndWait(t, sn) })

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
	t.Cleanup(func() { deleteAndWait(t, sn) })

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
	t.Cleanup(func() { deleteAndWait(t, sn) })

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
	t.Cleanup(func() { deleteAndWait(t, sn) })

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

// TestFinalizerLifecycle checks (with the fake client) that reconcile adds
// the registrar-cleanup finalizer, and that deletion deregisters the SIG
// set (manual backend: no-op) and removes the finalizer so the object can
// go away.
func TestFinalizerLifecycle(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	sn := newScionNetwork()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sn).
		WithStatusSubresource(&v1alpha1.ScionNetwork{}).
		Build()
	r := &ScionNetworkReconciler{Client: c, Scheme: scheme, AgentImage: testAgentImage}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	got := &v1alpha1.ScionNetwork{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Finalizers, registrarFinalizer) {
		t.Fatalf("finalizer not added: %v", got.Finalizers)
	}

	if err := c.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// With the finalizer removed the fake client deletes the object.
	err := c.Get(context.Background(), req.NamespacedName, &v1alpha1.ScionNetwork{})
	if err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("object still present after finalization: %v", err)
	}
}

// newDeletingScionNetwork builds a ScionNetwork already marked for
// deletion (finalizer + deletionTimestamp preset for the fake client, which
// permits deletionTimestamp only alongside finalizers). age shifts the
// deletionTimestamp into the past to exercise the deregistration deadline.
func newDeletingScionNetwork(age time.Duration) *v1alpha1.ScionNetwork {
	sn := newScionNetwork()
	sn.Finalizers = []string{registrarFinalizer}
	ts := metav1.NewTime(time.Now().Add(-age))
	sn.DeletionTimestamp = &ts
	return sn
}

func newFakeClient(t *testing.T, objs ...client.Object) (client.Client, *runtime.Scheme) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.ScionNetwork{}).
		Build()
	return c, scheme
}

// TestFinalizeAnapayaReleasesImmediately: the anapaya backend is an
// unimplemented stub (Ensure always returns ErrNotImplemented); nothing was
// ever registered, so deletion must release the finalizer on the first
// reconcile instead of wedging forever.
func TestFinalizeAnapayaReleasesImmediately(t *testing.T) {
	sn := newDeletingScionNetwork(0)
	sn.Spec.Registrar.Backend = "anapaya"
	c, scheme := newFakeClient(t, sn)
	r := &ScionNetworkReconciler{Client: c, Scheme: scheme, AgentImage: testAgentImage}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	err := c.Get(context.Background(), req.NamespacedName, &v1alpha1.ScionNetwork{})
	if err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("object still present after anapaya finalization: %v", err)
	}
}

// TestFinalizeHTTPFailureRetriesThenReleases: a reachable but erroring HTTP
// registrar must keep the finalizer (retry via error/backoff) while within
// the deregistration deadline, and release it once the deletionTimestamp is
// older than the deadline.
func TestFinalizeHTTPFailureRetriesThenReleases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}

	// Fresh deletion: still within the deadline, must retry (error) and
	// keep the finalizer.
	fresh := newDeletingScionNetwork(0)
	fresh.Spec.Registrar.Backend = "http"
	fresh.Spec.Registrar.Endpoint = srv.URL
	c, scheme := newFakeClient(t, fresh)
	r := &ScionNetworkReconciler{Client: c, Scheme: scheme, AgentImage: testAgentImage}
	if _, err := r.Reconcile(context.Background(), req); err == nil ||
		!strings.Contains(err.Error(), "deregister SIGs") {
		t.Fatalf("Reconcile error = %v, want deregister failure", err)
	}
	got := &v1alpha1.ScionNetwork{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatalf("object gone before deadline: %v", err)
	}
	if !slices.Contains(got.Finalizers, registrarFinalizer) {
		t.Fatalf("finalizer dropped before deadline: %v", got.Finalizers)
	}

	// Deletion older than the deadline: give up loudly and release.
	old := newDeletingScionNetwork(deregistrationDeadline + time.Minute)
	old.Spec.Registrar.Backend = "http"
	old.Spec.Registrar.Endpoint = srv.URL
	c, scheme = newFakeClient(t, old)
	r = &ScionNetworkReconciler{Client: c, Scheme: scheme, AgentImage: testAgentImage}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile past deadline: %v", err)
	}
	err := c.Get(context.Background(), req.NamespacedName, &v1alpha1.ScionNetwork{})
	if err == nil || !apierrors.IsNotFound(err) {
		t.Fatalf("object still present past deadline: %v", err)
	}
}

func TestIsIPv4CIDR(t *testing.T) {
	for _, tc := range []struct {
		cidr string
		want bool
	}{
		{"10.128.0.0/14", true},
		{"fd01::/48", false},
		{"not-a-cidr", false},
	} {
		if got := isIPv4CIDR(tc.cidr); got != tc.want {
			t.Errorf("isIPv4CIDR(%q) = %t, want %t", tc.cidr, got, tc.want)
		}
	}
}

func TestClusterForbiddenCIDRsIncludesUnderlay(t *testing.T) {
	sn := newScionNetwork()
	sn.Spec.AcceptPolicy.UnderlayCIDRs = []string{"192.168.111.0/24", "fd00::/64"}
	c, scheme := newFakeClient(t)
	r := &ScionNetworkReconciler{Client: c, Scheme: scheme}
	got, err := r.clusterForbiddenCIDRs(context.Background(), sn)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "192.168.111.0/24") {
		t.Fatalf("forbidden CIDRs = %v, want underlay IPv4 prefix", got)
	}
	if slices.Contains(got, "fd00::/64") {
		t.Fatalf("forbidden CIDRs = %v, IPv6 prefix should be filtered", got)
	}
}

func TestClusterForbiddenCIDRsRejectsInvalidSpecEntries(t *testing.T) {
	// Values that pass the CRD pattern but are not valid CIDRs must fail
	// the reconcile instead of being silently dropped from the deny list.
	for _, tc := range []struct {
		name   string
		mutate func(*v1alpha1.ScionNetwork)
		want   string
	}{
		{
			name: "forbidden octet out of range",
			mutate: func(sn *v1alpha1.ScionNetwork) {
				sn.Spec.AcceptPolicy.ForbiddenCIDRs = []string{"300.1.1.1/8"}
			},
			want: "spec.acceptPolicy.forbiddenCIDRs",
		},
		{
			name: "underlay prefix length out of range",
			mutate: func(sn *v1alpha1.ScionNetwork) {
				sn.Spec.AcceptPolicy.UnderlayCIDRs = []string{"10.0.0.0/33"}
			},
			want: "spec.acceptPolicy.underlayCIDRs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sn := newScionNetwork()
			tc.mutate(sn)
			c, scheme := newFakeClient(t)
			r := &ScionNetworkReconciler{Client: c, Scheme: scheme}
			_, err := r.clusterForbiddenCIDRs(context.Background(), sn)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("clusterForbiddenCIDRs error = %v, want mention of %s", err, tc.want)
			}
		})
	}
}

// TestUpdateStatusPlatformGating: a platform prerequisite failure
// (ConditionFalse) must block Available and drive the Degraded message even
// when all agents are ready, while an unverified platform (ConditionUnknown)
// must not.
func TestUpdateStatusPlatformGating(t *testing.T) {
	newObjs := func() (*v1alpha1.ScionNetwork, []client.Object) {
		sn := newScionNetwork()
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: render.Namespace,
				Name:      "agent-1",
				Labels:    map[string]string{"app": "scion-node-agent"},
			},
			Spec: corev1.PodSpec{NodeName: "node1"},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			}},
		}
		ds := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: render.Namespace, Name: "scion-node-agent"},
			Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 1},
		}
		return sn, []client.Object{sn, pod, ds}
	}
	cond := func(sn *v1alpha1.ScionNetwork, condType string) *metav1.Condition {
		for i := range sn.Status.Conditions {
			if sn.Status.Conditions[i].Type == condType {
				return &sn.Status.Conditions[i]
			}
		}
		return nil
	}

	t.Run("platform false blocks availability", func(t *testing.T) {
		sn, objs := newObjs()
		c, scheme := newFakeClient(t, objs...)
		r := &ScionNetworkReconciler{Client: c, Scheme: scheme}
		platform := podEgressPlatform{
			Status:  metav1.ConditionFalse,
			Reason:  "HostRoutingDisabled",
			Message: "OVN-Kubernetes shared-gateway mode bypasses host routes",
		}
		available, err := r.updateStatus(context.Background(), sn, v1alpha1.RegistrarStatus{}, false, platform)
		if err != nil {
			t.Fatal(err)
		}
		if available {
			t.Fatal("available = true, want false when platform prerequisite fails")
		}
		av := cond(sn, "Available")
		if av == nil || av.Status != metav1.ConditionFalse || av.Reason != "HostRoutingDisabled" ||
			av.Message != platform.Message {
			t.Fatalf("Available condition = %+v, want False/HostRoutingDisabled with platform message", av)
		}
		dg := cond(sn, "Degraded")
		if dg == nil || dg.Status != metav1.ConditionTrue || dg.Reason != "HostRoutingDisabled" ||
			dg.Message != platform.Message {
			t.Fatalf("Degraded condition = %+v, want True/HostRoutingDisabled with platform message", dg)
		}
	})

	t.Run("platform unknown does not block availability", func(t *testing.T) {
		sn, objs := newObjs()
		c, scheme := newFakeClient(t, objs...)
		r := &ScionNetworkReconciler{Client: c, Scheme: scheme}
		platform := podEgressPlatform{
			Status: metav1.ConditionUnknown,
			Reason: "PlatformUnverified",
		}
		available, err := r.updateStatus(context.Background(), sn, v1alpha1.RegistrarStatus{}, false, platform)
		if err != nil {
			t.Fatal(err)
		}
		if !available {
			t.Fatal("available = false, want true when platform is merely unverified")
		}
		av := cond(sn, "Available")
		if av == nil || av.Status != metav1.ConditionTrue || av.Reason != "NodeAgents" {
			t.Fatalf("Available condition = %+v, want True/NodeAgents", av)
		}
		dg := cond(sn, "Degraded")
		if dg == nil || dg.Status != metav1.ConditionFalse {
			t.Fatalf("Degraded condition = %+v, want False", dg)
		}
	})
}
