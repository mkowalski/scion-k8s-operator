package metricsauth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func newFake(authenticated, allowed bool) *fake.Clientset {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "tokenreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{
				Authenticated: authenticated,
				User:          authnv1.UserInfo{Username: "system:serviceaccount:ns:scraper"},
			}}, nil
		})
	cs.PrependReactor("create", "subjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{
				Allowed: allowed,
			}}, nil
		})
	return cs
}

func get(t *testing.T, h http.Handler, path, token string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("valid token allowed", func(t *testing.T) {
		h := Middleware(next, newFake(true, true))
		if code := get(t, h, "/metrics", "tok"); code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	})
	t.Run("missing token unauthorized", func(t *testing.T) {
		h := Middleware(next, newFake(true, true))
		if code := get(t, h, "/metrics", ""); code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", code)
		}
	})
	t.Run("invalid token unauthorized", func(t *testing.T) {
		h := Middleware(next, newFake(false, true))
		if code := get(t, h, "/metrics", "bad"); code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", code)
		}
	})
	t.Run("authenticated but not allowed forbidden", func(t *testing.T) {
		h := Middleware(next, newFake(true, false))
		if code := get(t, h, "/metrics", "tok"); code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", code)
		}
	})
	t.Run("probe paths bypass auth", func(t *testing.T) {
		h := Middleware(next, newFake(false, false), "/healthz", "/readyz")
		for _, p := range []string{"/healthz", "/readyz"} {
			if code := get(t, h, p, ""); code != http.StatusOK {
				t.Fatalf("%s status = %d, want 200 without auth", p, code)
			}
		}
	})
	t.Run("positive decision cached", func(t *testing.T) {
		cs := newFake(true, true)
		h := Middleware(next, cs)
		get(t, h, "/metrics", "tok")
		get(t, h, "/metrics", "tok")
		reviews := 0
		for _, a := range cs.Actions() {
			if a.GetResource().Resource == "tokenreviews" {
				reviews++
			}
		}
		if reviews != 1 {
			t.Fatalf("tokenreviews = %d, want 1 (second request served from cache)", reviews)
		}
	})
}
