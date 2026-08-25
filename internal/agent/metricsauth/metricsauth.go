// Package metricsauth guards the agent's metrics/debug endpoints with
// Kubernetes-native authentication and authorization, mirroring what
// kube-rbac-proxy does: the scraper's Bearer token is validated with a
// TokenReview, then a SubjectAccessReview checks that the caller may `get`
// the /metrics non-resource URL. Probe endpoints stay unauthenticated.
package metricsauth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// cacheTTL bounds how long a positive auth decision is reused, keeping
// apiserver load at one TokenReview+SAR pair per scraper per TTL while
// limiting the window in which a revoked token still works.
const cacheTTL = 2 * time.Minute

// cacheMax caps remembered tokens; beyond it the cache is reset (the
// expected population is one or two Prometheus scrapers).
const cacheMax = 128

// Middleware wraps next with TokenReview+SubjectAccessReview auth. Paths
// listed in unauthenticated bypass auth entirely (liveness/readiness probes
// must not depend on the apiserver).
func Middleware(next http.Handler, cs kubernetes.Interface, unauthenticated ...string) http.Handler {
	skip := map[string]bool{}
	for _, p := range unauthenticated {
		skip[p] = true
	}
	a := &authenticator{cs: cs, allowed: map[string]time.Time{}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		status, err := a.check(r.Context(), token)
		if err != nil {
			http.Error(w, "authentication check failed", http.StatusInternalServerError)
			return
		}
		if status != http.StatusOK {
			http.Error(w, http.StatusText(status), status)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type authenticator struct {
	cs      kubernetes.Interface
	mu      sync.Mutex
	allowed map[string]time.Time
}

// check returns the HTTP status for the token: 200, 401 (invalid token),
// or 403 (authenticated but not allowed to read /metrics).
func (a *authenticator) check(ctx context.Context, token string) (int, error) {
	a.mu.Lock()
	if exp, ok := a.allowed[token]; ok && time.Now().Before(exp) {
		a.mu.Unlock()
		return http.StatusOK, nil
	}
	a.mu.Unlock()

	tr, err := a.cs.AuthenticationV1().TokenReviews().Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token},
	}, metav1.CreateOptions{})
	if err != nil {
		return 0, err
	}
	if !tr.Status.Authenticated {
		return http.StatusUnauthorized, nil
	}
	sar, err := a.cs.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   tr.Status.User.Username,
			Groups: tr.Status.User.Groups,
			UID:    tr.Status.User.UID,
			NonResourceAttributes: &authzv1.NonResourceAttributes{
				Path: "/metrics", Verb: "get",
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return 0, err
	}
	if !sar.Status.Allowed {
		return http.StatusForbidden, nil
	}
	a.mu.Lock()
	if len(a.allowed) >= cacheMax {
		a.allowed = map[string]time.Time{}
	}
	a.allowed[token] = time.Now().Add(cacheTTL)
	a.mu.Unlock()
	return http.StatusOK, nil
}
