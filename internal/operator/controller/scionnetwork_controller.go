// Package controller reconciles ScionNetwork into the node-agent DaemonSet
// and supporting objects (Namespace, RBAC, optional SCC) and aggregates
// per-node agent readiness into status.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
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
	"github.com/mkowalski/scion-k8s-operator/internal/operator/registrar"
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

	forbidden, openshift, err := r.clusterForbiddenCIDRs(ctx, sn)
	if err != nil {
		r.markApplyFailed(ctx, sn, err)
		return ctrl.Result{}, err
	}
	if err := r.validateNodeIPAdvertisement(ctx, sn); err != nil {
		r.markApplyFailed(ctx, sn, err)
		return ctrl.Result{}, err
	}
	// The platform probe is informational and never returns an error: any
	// failure is reflected in the podEgressPlatform condition instead of
	// blocking the data-plane apply below.
	platform := r.detectPodEgressPlatform(ctx)
	image := sn.Spec.AgentImage
	if image == "" {
		image = r.AgentImage
	}

	if err := r.apply(ctx, sn, image, forbidden, openshift); err != nil {
		r.markApplyFailed(ctx, sn, err)
		return ctrl.Result{}, err
	}

	// Registrar sync failure must not fail the whole reconcile: the data
	// plane keeps working without AS-side registration. The failure is
	// surfaced in status (and Degraded for non-manual backends) and
	// retried via RequeueAfter below.
	regStatus, regFailed := r.syncRegistrar(ctx, sn)

	r.discoverISDAS(ctx, sn)

	available, err := r.updateStatus(ctx, sn, regStatus, regFailed, platform)
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
	if platform.Status != metav1.ConditionTrue {
		// The optional platform APIs are only watched when their CRDs
		// exist at operator startup (see SetupWithManager). If they are
		// installed later, no event fires, so poll slowly to converge
		// the podEgressPlatform condition instead of staying Unknown
		// until an unrelated event or a restart.
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	return ctrl.Result{}, nil
}

// deregistrationDeadline bounds how long a persistently failing registrar
// Ensure may block ScionNetwork deletion, measured from the object's
// deletionTimestamp. See finalize for the escape-hatch rationale (also
// documented in docs/known-gaps.md).
const deregistrationDeadline = 10 * time.Minute

// finalize deregisters all node SIGs from the AS-side registrar (an Ensure
// with an empty set) and then removes the finalizer. Blocking deletion
// forever on a broken registrar is worse than leaving stale entries, which
// the AS side can garbage collect, so the finalizer is dropped anyway when:
//
//   - backend construction fails (e.g. the credentials Secret was already
//     deleted);
//   - the backend is an unimplemented stub (ErrNotImplemented, e.g.
//     anapaya) — a stub never registered anything, so there is nothing to
//     deregister;
//   - Ensure keeps failing past deregistrationDeadline after the
//     deletionTimestamp — logged loudly; until then failures are retried
//     with the controller's backoff.
//
// Manual escape hatch (documented in docs/known-gaps.md): remove the
// finalizer by hand, e.g.
// kubectl patch scionnetwork cluster --type=merge -p '{"metadata":{"finalizers":null}}'.
func (r *ScionNetworkReconciler) finalize(ctx context.Context, sn *v1alpha1.ScionNetwork) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sn, registrarFinalizer) {
		return ctrl.Result{}, nil
	}
	log := ctrl.LoggerFrom(ctx)
	backend, err := r.backendFor(ctx, sn)
	if err != nil {
		log.Error(err,
			"registrar backend unavailable during finalization; skipping deregistration")
	} else {
		ectx, cancel := context.WithTimeout(ctx, registrarTimeout)
		defer cancel()
		switch err := backend.Ensure(ectx, nil); {
		case err == nil:
		case errors.Is(err, registrar.ErrNotImplemented):
			log.Info("registrar backend is an unimplemented stub; nothing was ever registered, skipping deregistration",
				"backend", sn.Spec.Registrar.Backend)
		case time.Since(sn.DeletionTimestamp.Time) > deregistrationDeadline:
			log.Error(err,
				"SIG deregistration still failing past the deletion deadline; releasing the finalizer WITHOUT deregistering — stale SIG entries may remain on the AS side",
				"deadline", deregistrationDeadline.String(),
				"deletionTimestamp", sn.DeletionTimestamp.String())
		default:
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
func (r *ScionNetworkReconciler) apply(ctx context.Context, sn *v1alpha1.ScionNetwork, image string, forbidden []string, metricsTLS bool) error {
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

	ds := render.DaemonSet(sn, image, forbidden, metricsTLS)
	want := render.DaemonSet(sn, image, forbidden, metricsTLS)
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

// clusterForbiddenCIDRs merges IPv4 entries from
// spec.acceptPolicy.{forbiddenCIDRs,underlayCIDRs} with the cluster's IPv4 pod
// and service networks read from the OpenShift
// network.config.openshift.io "cluster" object. The agent policy engine is
// IPv4-only; dual-stack clusters therefore contribute only their IPv4 ranges.
// On vanilla Kubernetes the GET fails with NotFound or NoMatch and spec values
// are used as-is.
//
// Spec-provided entries are deny-list input: an unparsable value must fail
// the reconcile (surfacing as Degraded/ApplyFailed) rather than be silently
// dropped, which would widen the accepted address space. Only valid IPv6
// prefixes are skipped silently. Cluster-derived values are filtered to IPv4
// without error, since dual-stack clusters legitimately carry IPv6 ranges.
// The second return reports whether the OpenShift network config API was
// found, which the caller uses as the OpenShift signal (e.g. to enable
// service-ca-backed metrics TLS).
func (r *ScionNetworkReconciler) clusterForbiddenCIDRs(ctx context.Context, sn *v1alpha1.ScionNetwork) ([]string, bool, error) {
	out, err := specIPv4CIDRs("forbiddenCIDRs", sn.Spec.AcceptPolicy.ForbiddenCIDRs)
	if err != nil {
		return nil, false, err
	}
	underlay, err := specIPv4CIDRs("underlayCIDRs", sn.Spec.AcceptPolicy.UnderlayCIDRs)
	if err != nil {
		return nil, false, err
	}
	out = append(out, underlay...)

	nc := &unstructured.Unstructured{}
	nc.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "config.openshift.io", Version: "v1", Kind: "Network",
	})
	err = r.Get(ctx, types.NamespacedName{Name: "cluster"}, nc)
	switch {
	case err == nil:
		cns, _, _ := unstructured.NestedSlice(nc.Object, "spec", "clusterNetwork")
		for _, cn := range cns {
			if m, ok := cn.(map[string]interface{}); ok {
				if cidr, ok := m["cidr"].(string); ok && isIPv4CIDR(cidr) {
					out = append(out, cidr)
				}
			}
		}
		svc, _, _ := unstructured.NestedStringSlice(nc.Object, "spec", "serviceNetwork")
		for _, cidr := range svc {
			if isIPv4CIDR(cidr) {
				out = append(out, cidr)
			}
		}
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		// Vanilla Kubernetes: no OpenShift network config.
		return dedupe(out), false, nil
	default:
		return nil, false, fmt.Errorf("read openshift network config: %w", err)
	}
	return dedupe(out), true, nil
}

// specIPv4CIDRs validates user-provided CIDR entries from the ScionNetwork
// spec. Unparsable entries (which the CRD pattern cannot fully reject, e.g.
// "300.1.1.1/8" or "10.0.0.0/33") are an error; valid IPv6 prefixes are
// skipped because the agent policy engine is IPv4-only.
func specIPv4CIDRs(field string, cidrs []string) ([]string, error) {
	var out []string
	for _, cidr := range cidrs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q in spec.acceptPolicy.%s: %w", cidr, field, err)
		}
		if prefix.Addr().Is4() {
			out = append(out, cidr)
		}
	}
	return out, nil
}

// validateNodeIPAdvertisement refuses an explicitly enabled
// advertisement.nodeIP when any selected node's InternalIP falls inside
// spec.acceptPolicy.underlayCIDRs: advertising such a /32 makes the remote
// SIG route the SCION underlay itself into the tunnel (verified live: a
// routing loop that blackholed probes and discovery). Fail-closed like the
// CIDR validation: the error surfaces as Degraded/ApplyFailed.
func (r *ScionNetworkReconciler) validateNodeIPAdvertisement(ctx context.Context, sn *v1alpha1.ScionNetwork) error {
	if sn.Spec.Advertisement.NodeIP == nil || !*sn.Spec.Advertisement.NodeIP {
		return nil
	}
	var prefixes []netip.Prefix
	for _, cidr := range sn.Spec.AcceptPolicy.UnderlayCIDRs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			// clusterForbiddenCIDRs already rejected it with a better message.
			continue
		}
		prefixes = append(prefixes, prefix)
	}
	if len(prefixes) == 0 {
		return nil
	}
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes, client.MatchingLabels(sn.Spec.NodeSelector)); err != nil {
		return fmt.Errorf("list nodes for nodeIP advertisement check: %w", err)
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		for _, a := range n.Status.Addresses {
			if a.Type != corev1.NodeInternalIP {
				continue
			}
			ip, err := netip.ParseAddr(a.Address)
			if err != nil {
				continue
			}
			for _, prefix := range prefixes {
				if prefix.Contains(ip) {
					return fmt.Errorf(
						"advertisement.nodeIP is enabled but node %s IP %s is inside underlay CIDR %s; "+
							"advertising it would route the SCION underlay into the tunnel (routing loop) — "+
							"disable spec.advertisement.nodeIP or move node IPs off the underlay",
						n.Name, a.Address, prefix)
				}
			}
		}
	}
	return nil
}

func isIPv4CIDR(cidr string) bool {
	prefix, err := netip.ParsePrefix(cidr)
	return err == nil && prefix.Addr().Is4()
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

// updateStatus aggregates agent readiness, the platform routing prerequisite,
// and registrar state. Returns whether the complete service is available.
func (r *ScionNetworkReconciler) updateStatus(
	ctx context.Context,
	sn *v1alpha1.ScionNetwork,
	reg v1alpha1.RegistrarStatus,
	regFailed bool,
	platform podEgressPlatform,
) (bool, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(render.Namespace),
		client.MatchingLabels{"app": "scion-node-agent"}); err != nil {
		return false, err
	}
	var ready int32
	var degraded []string
	now := time.Now()
	for i := range pods.Items {
		p := &pods.Items[i]
		if podReady(p) {
			ready++
			continue
		}
		// Grace period: pods routinely cycle through unready during
		// rollouts and node reboots; only pods unready beyond the grace
		// window mark the network Degraded (Available/Progressing still
		// reflect them immediately, and the !available requeue re-checks
		// every 30s until the grace expires or the pod recovers).
		if now.Sub(unreadySince(p)) < unreadyGracePeriod {
			continue
		}
		name := p.Spec.NodeName
		if name == "" {
			name = p.Name
		}
		degraded = append(degraded, name)
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

	agentsAvailable := total > 0 && ready == total
	available := agentsAvailable && platform.Status != metav1.ConditionFalse
	progressing := !dsCurrent || total == 0 || ready < total
	isDegraded, reason, degradedMsg := degradedReason(len(degraded),
		fmt.Sprintf("nodes unready for over %v: %v", unreadyGracePeriod, degraded),
		platform, regFailed, reg.LastError, sn.Spec.Registrar.Backend)

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
	availableReason := "NodeAgents"
	availableMessage := fmt.Sprintf("%d/%d node agents ready", ready, total)
	if agentsAvailable && platform.Status == metav1.ConditionFalse {
		availableReason = platform.Reason
		availableMessage = platform.Message
	}
	setCond("Available", available, availableReason, availableMessage)
	setCond("Progressing", progressing, "Rollout",
		fmt.Sprintf("%d/%d node agents ready", ready, total))
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

// unreadyGracePeriod is how long an agent pod may be unready (rollouts,
// node reboots) before it marks the ScionNetwork Degraded. Nodes missing a
// pod entirely are not age-tracked; they surface through Available=False
// and Progressing=True instead.
const unreadyGracePeriod = 5 * time.Minute

// unreadySince returns when the pod last transitioned out of Ready, falling
// back to the creation timestamp for pods that never published a Ready
// condition (e.g. stuck in image pull).
func unreadySince(p *corev1.Pod) time.Time {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.LastTransitionTime.Time
		}
	}
	return p.CreationTimestamp.Time
}

// SetupWithManager registers the controller: reconciles the singleton on
// ScionNetwork and owned-DaemonSet changes, and re-enqueues "cluster" on Node
// events and on the optional OpenShift routing APIs when they are discoverable.
// Agent pod readiness is not watched directly (pods are owned by the
// DaemonSet); Reconcile requeues every 30s until fully available instead.
func (r *ScionNetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	enqueueCluster := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, _ client.Object) []ctrl.Request {
			return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: "cluster"}}}
		})
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ScionNetwork{}).
		Owns(&appsv1.DaemonSet{}).
		Watches(&corev1.Node{}, enqueueCluster)

	optionalWatch := func(group, version, kind string) {
		if _, err := mgr.GetRESTMapper().RESTMapping(schema.GroupKind{Group: group, Kind: kind}, version); err != nil {
			return
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
		builder.Watches(obj, enqueueCluster)
	}
	optionalWatch("operator.openshift.io", "v1", "Network")
	optionalWatch("k8s.ovn.org", "v1", "RouteAdvertisements")
	return builder.Complete(r)
}
