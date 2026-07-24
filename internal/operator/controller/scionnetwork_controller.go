// Package controller reconciles ScionNetwork into the node-agent DaemonSet
// and supporting objects (Namespace, RBAC, optional SCC) and aggregates
// per-node agent readiness into status.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	v1alpha1 "github.com/mkowalski/scion-k8s-operator/api/v1alpha1"
	"github.com/mkowalski/scion-k8s-operator/internal/operator/render"
)

// degradedGauge is 1 while the ScionNetwork Degraded condition is True.
// The name is relied upon by the PrometheusRule shipped in config/manifests.
var degradedGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "scion_operator_scionnetwork_degraded",
	Help: "1 if the ScionNetwork Degraded condition is true, else 0.",
})

func init() {
	metrics.Registry.MustRegister(degradedGauge)
}

// ScionNetworkReconciler reconciles the cluster-scoped ScionNetwork
// singleton.
type ScionNetworkReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// AgentImage is the default agent image (env AGENT_IMAGE), used when
	// spec.agentImage is empty.
	AgentImage string
	// SCCAvail is true when security.openshift.io/v1 was discovered at
	// startup; the SCC object is only applied then.
	SCCAvail bool

	// bootstrapHTTP fetches <discoveryURL>/topology for status.isdAS;
	// lazily initialized (10s timeout) and reused across reconciles.
	bootstrapHTTP *http.Client
}

// registrarFinalizer guards ScionNetwork deletion until the node SIG set
// has been deregistered from the AS-side registrar.
const registrarFinalizer = "scion.mkowalski.github.io/registrar-cleanup"

func (r *ScionNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	sn := &v1alpha1.ScionNetwork{}
	if err := r.Get(ctx, req.NamespacedName, sn); err != nil {
		// Not-found: dependents are garbage-collected via owner refs.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !sn.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, sn)
	}
	if !controllerutil.ContainsFinalizer(sn, registrarFinalizer) {
		controllerutil.AddFinalizer(sn, registrarFinalizer)
		if err := r.Update(ctx, sn); err != nil {
			return ctrl.Result{}, err
		}
	}

	forbidden, err := r.clusterForbiddenCIDRs(ctx, sn)
	if err != nil {
		r.markApplyFailed(ctx, sn, err)
		return ctrl.Result{}, err
	}
	image := sn.Spec.AgentImage
	if image == "" {
		image = r.AgentImage
	}

	if err := r.apply(ctx, sn, image, forbidden); err != nil {
		r.markApplyFailed(ctx, sn, err)
		return ctrl.Result{}, err
	}

	// Registrar sync failure must not fail the whole reconcile: the data
	// plane keeps working without AS-side registration. The failure is
	// surfaced in status (and Degraded for non-manual backends) and
	// retried via RequeueAfter below.
	regStatus, regFailed := r.syncRegistrar(ctx, sn)

	r.discoverISDAS(ctx, sn)

	available, err := r.updateStatus(ctx, sn, regStatus, regFailed)
	if err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	if regFailed {
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}
	if !available {
		// Pod readiness changes do not trigger this controller (pods are
		// owned by the DaemonSet, not the ScionNetwork), so poll until
		// fully available.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// finalize deregisters all node SIGs from the AS-side registrar (an Ensure
// with an empty set) and then removes the finalizer. Backend construction
// failures (e.g. the credentials Secret was already deleted) drop the
// finalizer anyway: blocking deletion forever on a lost secret is worse
// than leaving stale registrar entries, which the AS side can garbage
// collect. Ensure failures are retried.
func (r *ScionNetworkReconciler) finalize(ctx context.Context, sn *v1alpha1.ScionNetwork) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sn, registrarFinalizer) {
		return ctrl.Result{}, nil
	}
	backend, err := r.backendFor(ctx, sn)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err,
			"registrar backend unavailable during finalization; skipping deregistration")
	} else {
		ectx, cancel := context.WithTimeout(ctx, registrarTimeout)
		defer cancel()
		if err := backend.Ensure(ectx, nil); err != nil {
			return ctrl.Result{}, fmt.Errorf("deregister SIGs: %w", err)
		}
	}
	controllerutil.RemoveFinalizer(sn, registrarFinalizer)
	if err := r.Update(ctx, sn); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// discoverISDAS populates status.ISDAS from the bootstrap discovery server
// (mutating sn only; the caller persists status). Only the "url" bootstrap
// mode is handled: for dns/dhcp/mdns discovery happens on the nodes, so the
// operator has no discovery path of its own (future: agent-reported ISD-AS).
// The value is fetched only while status.ISDAS is empty — a discovered value
// is kept as-is even if spec.bootstrap later changes, which keeps reconciles
// cheap; clearing status (or recreating the CR) forces a refetch. Fetch or
// parse failures leave the previous value untouched and never fail the
// reconcile: the ISD-AS is informational and the discovery server may be
// temporarily unreachable while the data plane keeps working.
func (r *ScionNetworkReconciler) discoverISDAS(ctx context.Context, sn *v1alpha1.ScionNetwork) {
	if sn.Spec.Bootstrap.Mode != "url" || sn.Status.ISDAS != "" {
		return
	}
	if r.bootstrapHTTP == nil {
		r.bootstrapHTTP = &http.Client{Timeout: 10 * time.Second}
	}
	url := strings.TrimRight(sn.Spec.Bootstrap.DiscoveryURL, "/") + "/topology"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	resp, err := r.bootstrapHTTP.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return
	}
	var topo struct {
		IA string `json:"isd_as"`
	}
	if err := json.Unmarshal(body, &topo); err != nil || topo.IA == "" {
		return
	}
	sn.Status.ISDAS = topo.IA
}

// markApplyFailed surfaces a persistent apply/config-read failure as a
// Degraded=True condition. Best-effort: the status-update error is ignored
// because the caller returns the original error, which triggers a retry.
func (r *ScionNetworkReconciler) markApplyFailed(ctx context.Context, sn *v1alpha1.ScionNetwork, applyErr error) {
	meta.SetStatusCondition(&sn.Status.Conditions, metav1.Condition{
		Type: "Degraded", Status: metav1.ConditionTrue,
		Reason: "ApplyFailed", Message: applyErr.Error(),
		ObservedGeneration: sn.Generation,
	})
	_ = r.Status().Update(ctx, sn)
}

// apply creates or updates all managed objects. Every object except the
// Namespace carries an owner reference to the ScionNetwork; a cluster-scoped
// owner is valid for both cluster-scoped and namespaced dependents.
func (r *ScionNetworkReconciler) apply(ctx context.Context, sn *v1alpha1.ScionNetwork, image string, forbidden []string) error {
	// Namespace first: the namespaced objects need it. Deliberately NO
	// owner reference here: the operator Deployment itself runs in
	// scion-system (shipped by config/manifests), so garbage-collecting the
	// namespace on ScionNetwork deletion would delete the operator too.
	// Namespace lifecycle is owned by the deploy manifests; the controller
	// only creates it if absent and keeps the PSA labels merged.
	ns := render.NamespaceObj()
	wantNSLabels := render.NamespaceObj().Labels
	if err := r.applyObjectNoOwner(ctx, ns, func() error {
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		for k, v := range wantNSLabels {
			ns.Labels[k] = v
		}
		return nil
	}); err != nil {
		return fmt.Errorf("apply namespace: %w", err)
	}

	sa := render.ServiceAccount()
	if err := r.applyObject(ctx, sn, sa, func() error { return nil }); err != nil {
		return fmt.Errorf("apply serviceaccount: %w", err)
	}

	cr := render.ClusterRole()
	wantRules := render.ClusterRole().Rules
	if err := r.applyObject(ctx, sn, cr, func() error {
		cr.Rules = wantRules
		return nil
	}); err != nil {
		return fmt.Errorf("apply clusterrole: %w", err)
	}

	// roleRef is immutable; it is only set at create time (via the rendered
	// object). Drift in roleRef would require delete+recreate, which we do
	// not attempt: the operator is the only writer of this binding.
	crb := render.ClusterRoleBinding()
	wantSubjects := render.ClusterRoleBinding().Subjects
	if err := r.applyObject(ctx, sn, crb, func() error {
		crb.Subjects = wantSubjects
		return nil
	}); err != nil {
		return fmt.Errorf("apply clusterrolebinding: %w", err)
	}

	ds := render.DaemonSet(sn, image, forbidden)
	want := render.DaemonSet(sn, image, forbidden)
	if err := r.applyObject(ctx, sn, ds, func() error {
		if ds.Labels == nil {
			ds.Labels = map[string]string{}
		}
		for k, v := range want.Labels {
			ds.Labels[k] = v
		}
		ds.Spec = want.Spec
		return nil
	}); err != nil {
		return fmt.Errorf("apply daemonset: %w", err)
	}

	if r.SCCAvail {
		scc := render.SCC()
		want := render.SCC()
		if err := r.applyObject(ctx, sn, scc, func() error {
			for k, v := range want.Object {
				if k == "metadata" {
					continue
				}
				scc.Object[k] = v
			}
			return nil
		}); err != nil {
			return fmt.Errorf("apply scc: %w", err)
		}
	}
	return nil
}

// applyObject sets the owner reference and runs CreateOrUpdate with mutate.
func (r *ScionNetworkReconciler) applyObject(ctx context.Context, sn *v1alpha1.ScionNetwork, obj client.Object, mutate func() error) error {
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		if err := ctrl.SetControllerReference(sn, obj, r.Scheme); err != nil {
			return err
		}
		return mutate()
	})
	return err
}

// applyObjectNoOwner runs CreateOrUpdate without setting an owner
// reference. Used for the scion-system Namespace, whose lifecycle is owned
// by the deploy manifests (the operator itself runs in it).
func (r *ScionNetworkReconciler) applyObjectNoOwner(ctx context.Context, obj client.Object, mutate func() error) error {
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, mutate)
	return err
}

// clusterForbiddenCIDRs merges spec.acceptPolicy.forbiddenCIDRs with the
// cluster's pod and service networks read from the OpenShift
// network.config.openshift.io "cluster" object. On vanilla Kubernetes the
// GET fails with NotFound or NoMatch and spec values are used as-is.
func (r *ScionNetworkReconciler) clusterForbiddenCIDRs(ctx context.Context, sn *v1alpha1.ScionNetwork) ([]string, error) {
	out := append([]string{}, sn.Spec.AcceptPolicy.ForbiddenCIDRs...)

	nc := &unstructured.Unstructured{}
	nc.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "config.openshift.io", Version: "v1", Kind: "Network",
	})
	err := r.Get(ctx, types.NamespacedName{Name: "cluster"}, nc)
	switch {
	case err == nil:
		cns, _, _ := unstructured.NestedSlice(nc.Object, "spec", "clusterNetwork")
		for _, cn := range cns {
			if m, ok := cn.(map[string]interface{}); ok {
				if cidr, ok := m["cidr"].(string); ok && cidr != "" {
					out = append(out, cidr)
				}
			}
		}
		svc, _, _ := unstructured.NestedStringSlice(nc.Object, "spec", "serviceNetwork")
		out = append(out, svc...)
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		// Vanilla Kubernetes: no OpenShift network config.
	default:
		return nil, fmt.Errorf("read openshift network config: %w", err)
	}
	return dedupe(out), nil
}

func dedupe(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// updateStatus aggregates agent pod readiness into status.nodes and the
// Available/Progressing/Degraded conditions, and persists the registrar
// status computed by syncRegistrar — one status write per reconcile. A
// registrar failure marks Degraded (reason RegistrarSyncFailed) only for
// non-manual backends; manual mode cannot fail. Returns whether the
// ScionNetwork is fully available.
func (r *ScionNetworkReconciler) updateStatus(ctx context.Context, sn *v1alpha1.ScionNetwork, reg v1alpha1.RegistrarStatus, regFailed bool) (bool, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(render.Namespace),
		client.MatchingLabels{"app": "scion-node-agent"}); err != nil {
		return false, err
	}
	var ready int32
	var degraded []string
	for i := range pods.Items {
		p := &pods.Items[i]
		if podReady(p) {
			ready++
		} else {
			name := p.Spec.NodeName
			if name == "" {
				name = p.Name
			}
			degraded = append(degraded, name)
		}
	}
	slices.Sort(degraded)

	// Total comes from the DaemonSet's DesiredNumberScheduled so nodes
	// that should run an agent but have no pod at all still count against
	// availability. If the DaemonSet is missing (e.g. just deleted) fall
	// back to the pod count. Note: status.nodes.degraded only names nodes
	// whose pod exists but is unready — nodes missing a pod entirely are
	// reflected in the ready/total gap but not named (the pod list does
	// not know which nodes they are).
	dsCurrent := false
	total := int32(len(pods.Items))
	ds := &appsv1.DaemonSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: render.Namespace, Name: "scion-node-agent"}, ds); err == nil {
		dsCurrent = ds.Status.ObservedGeneration == ds.Generation
		total = ds.Status.DesiredNumberScheduled
	}

	available := total > 0 && ready == total
	progressing := !dsCurrent || total == 0 || ready < total
	// TODO: refine Degraded to "pod unready for >5m" once pod transition
	// timestamps are tracked; for now any unready pod marks Degraded.
	isDegraded, reason := degradedReason(ready, total, regFailed, sn.Spec.Registrar.Backend)

	sn.Status.Nodes = v1alpha1.NodeSummary{Ready: ready, Total: total, Degraded: degraded}
	sn.Status.Registrar = reg
	setCond := func(t string, on bool, reason, msg string) {
		st := metav1.ConditionFalse
		if on {
			st = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&sn.Status.Conditions, metav1.Condition{
			Type: t, Status: st, Reason: reason, Message: msg,
			ObservedGeneration: sn.Generation,
		})
	}
	setCond("Available", available, "NodeAgents",
		fmt.Sprintf("%d/%d node agents ready", ready, total))
	setCond("Progressing", progressing, "Rollout",
		fmt.Sprintf("%d/%d node agents ready", ready, total))
	degradedMsg := fmt.Sprintf("unready nodes: %v", degraded)
	if reason == "RegistrarSyncFailed" {
		degradedMsg = reg.LastError
	}
	setCond("Degraded", isDegraded, reason, degradedMsg)

	if err := r.Status().Update(ctx, sn); err != nil {
		return false, err
	}
	// Flip the gauge only after status was persisted so the metric and the
	// observable condition agree.
	if isDegraded {
		degradedGauge.Set(1)
	} else {
		degradedGauge.Set(0)
	}
	return available, nil
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// SetupWithManager registers the controller: reconciles the singleton on
// ScionNetwork and owned-DaemonSet changes, and re-enqueues "cluster" on
// Node events (node add/remove changes the desired agent set). Agent pod
// readiness is not watched directly (pods are owned by the DaemonSet);
// Reconcile requeues every 30s until fully available instead.
func (r *ScionNetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	enqueueCluster := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, _ client.Object) []ctrl.Request {
			return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: "cluster"}}}
		})
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ScionNetwork{}).
		Owns(&appsv1.DaemonSet{}).
		Watches(&corev1.Node{}, enqueueCluster).
		Complete(r)
}
