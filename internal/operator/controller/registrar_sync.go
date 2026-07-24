package controller

// This file implements registrar sync: it builds the desired SIG set from
// the selected nodes and pushes it through the configured backend
// (manual/http/anapaya).

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/mkowalski/scion-k8s-operator/api/v1alpha1"
	"github.com/mkowalski/scion-k8s-operator/internal/operator/registrar"
	"github.com/mkowalski/scion-k8s-operator/internal/operator/render"
)

// SIG NodePort/hostPort endpoints exposed by the node agent.
const (
	sigCtrlPort = 30256
	sigDataPort = 30056
)

// registrarTimeout bounds one backend Ensure call so a hung AS-side
// endpoint cannot stall the reconcile loop.
const registrarTimeout = 30 * time.Second

// nodesToSIGs maps nodes matching selector to their SIG endpoints, sorted
// by node name for deterministic status output. Selected nodes without an
// InternalIP are dropped and returned as skipped names (the caller logs
// them); an empty selector matches all nodes, mirroring DaemonSet
// nodeSelector semantics.
func nodesToSIGs(nodes []corev1.Node, selector map[string]string) (sigs []registrar.SIG, skipped []string) {
	sel := labels.SelectorFromSet(selector)
	for i := range nodes {
		n := &nodes[i]
		if !sel.Matches(labels.Set(n.Labels)) {
			continue
		}
		ip := ""
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				ip = a.Address
				break
			}
		}
		if ip == "" {
			skipped = append(skipped, n.Name)
			continue
		}
		sigs = append(sigs, registrar.SIG{
			Name:     n.Name,
			CtrlAddr: fmt.Sprintf("%s:%d", ip, sigCtrlPort),
			DataAddr: fmt.Sprintf("%s:%d", ip, sigDataPort),
		})
	}
	slices.SortFunc(sigs, func(a, b registrar.SIG) int {
		return strings.Compare(a.Name, b.Name)
	})
	slices.Sort(skipped)
	return sigs, skipped
}

// backendFor selects the registrar backend from spec. For http, the bearer
// token is read from key "token" of the referenced Secret in scion-system.
func (r *ScionNetworkReconciler) backendFor(ctx context.Context, sn *v1alpha1.ScionNetwork) (registrar.Backend, error) {
	switch sn.Spec.Registrar.Backend {
	case "", "manual":
		return registrar.Manual{}, nil
	case "http":
		if ref := sn.Spec.Registrar.CredentialsSecretRef; ref != nil {
			sec := &corev1.Secret{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: render.Namespace, Name: ref.Name}, sec); err != nil {
				return nil, fmt.Errorf("read credentials secret %s/%s: %w", render.Namespace, ref.Name, err)
			}
			token, ok := sec.Data["token"]
			if !ok {
				return nil, fmt.Errorf("credentials secret %q has no \"token\" key", ref.Name)
			}
			return &registrar.HTTP{Endpoint: sn.Spec.Registrar.Endpoint, Token: string(token)}, nil
		}
		// No secret ref: send an empty token; the AS-side service fails
		// closed on empty tokens, surfacing the misconfiguration.
		return &registrar.HTTP{Endpoint: sn.Spec.Registrar.Endpoint}, nil
	case "anapaya":
		return registrar.Anapaya{}, nil
	default:
		return nil, fmt.Errorf("unknown registrar backend %q", sn.Spec.Registrar.Backend)
	}
}

// syncRegistrar computes the desired SIG set and pushes it through the
// configured backend. It returns the RegistrarStatus to persist (the caller
// writes status once per reconcile) and whether the sync failed. DesiredSIGs
// is always populated — manual-mode consumers copy it, and it aids
// debugging for the other backends. RegisteredNodes and LastSyncTime only
// advance on success; on failure the previous values are kept and LastError
// records the cause.
func (r *ScionNetworkReconciler) syncRegistrar(ctx context.Context, sn *v1alpha1.ScionNetwork) (v1alpha1.RegistrarStatus, bool) {
	st := sn.Status.Registrar

	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		st.LastError = fmt.Sprintf("list nodes: %v", err)
		return st, true
	}
	sigs, skipped := nodesToSIGs(nodes.Items, sn.Spec.NodeSelector)
	if len(skipped) > 0 {
		ctrl.LoggerFrom(ctx).Info("nodes without InternalIP skipped from SIG registration", "nodes", skipped)
	}

	desired := make([]string, 0, len(sigs))
	for _, s := range sigs {
		desired = append(desired, fmt.Sprintf("%s=%s,%s", s.Name, s.CtrlAddr, s.DataAddr))
	}
	st.DesiredSIGs = desired

	backend, err := r.backendFor(ctx, sn)
	if err == nil {
		ectx, cancel := context.WithTimeout(ctx, registrarTimeout)
		defer cancel()
		err = backend.Ensure(ectx, sigs)
	}
	if err != nil {
		st.LastError = err.Error()
		return st, true
	}
	st.RegisteredNodes = int32(len(sigs))
	st.LastSyncTime = &metav1.Time{Time: time.Now()}
	st.LastError = ""
	return st, false
}

// degradedReason decides the Degraded condition from agent readiness and
// registrar sync state. Unready agents take precedence as the reason; a
// registrar failure degrades only for non-manual backends (manual cannot
// meaningfully fail from the AS side).
func degradedReason(ready, total int32, regFailed bool, backend string) (bool, string) {
	regDegraded := regFailed && backend != "" && backend != "manual"
	if ready < total {
		return true, "UnreadyAgents"
	}
	if regDegraded {
		return true, "RegistrarSyncFailed"
	}
	return false, "UnreadyAgents"
}
