package registrar

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	registrar "github.com/mkowalski/scion-k8s-operator/internal/registrar"
)

// Interface compliance for all backends.
var (
	_ Backend = Manual{}
	_ Backend = (*HTTP)(nil)
	_ Backend = Anapaya{}
)

func TestManualEnsureIsNoOp(t *testing.T) {
	if err := (Manual{}).Ensure(context.Background(), []SIG{{Name: "n"}}); err != nil {
		t.Fatalf("Manual.Ensure = %v, want nil", err)
	}
}

func TestAnapayaEnsureNotImplemented(t *testing.T) {
	err := (Anapaya{}).Ensure(context.Background(), nil)
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Anapaya.Ensure = %v, want ErrNotImplemented", err)
	}
}

func TestHTTPEnsurePutsDesiredSet(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	h := &HTTP{Endpoint: srv.URL, Token: "s3cret"}
	err := h.Ensure(context.Background(), []SIG{
		{Name: "node1", CtrlAddr: "192.0.2.1:30256", DataAddr: "192.0.2.1:30056"},
		{Name: "node2", CtrlAddr: "192.0.2.2:30256", DataAddr: "192.0.2.2:30056"},
	})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/v1/sigs" {
		t.Errorf("path = %q, want /v1/sigs", gotPath)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("auth = %q, want Bearer s3cret", gotAuth)
	}
	want := map[string]map[string]string{
		"node1": {"ctrl_addr": "192.0.2.1:30256", "data_addr": "192.0.2.1:30056"},
		"node2": {"ctrl_addr": "192.0.2.2:30256", "data_addr": "192.0.2.2:30056"},
	}
	if len(gotBody) != len(want) {
		t.Fatalf("body = %v, want %v", gotBody, want)
	}
	for name, w := range want {
		g := gotBody[name]
		if g["ctrl_addr"] != w["ctrl_addr"] || g["data_addr"] != w["data_addr"] {
			t.Errorf("body[%s] = %v, want %v", name, g, w)
		}
	}
}

func TestHTTPEnsureEmptySetSendsEmptyObject(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		raw = strings.TrimSpace(string(b[:n]))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := (&HTTP{Endpoint: srv.URL, Token: "t"}).Ensure(context.Background(), nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if raw != "{}" {
		t.Errorf("body = %q, want {}", raw)
	}
}

func TestHTTPEnsureNon204IsError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, "unauthorized"},
		{"reload failed bodyless", http.StatusBadGateway, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, _ = w.Write([]byte(tc.body))
				}
			}))
			defer srv.Close()

			err := (&HTTP{Endpoint: srv.URL, Token: "t"}).Ensure(context.Background(), []SIG{{Name: "n"}})
			if err == nil {
				t.Fatal("Ensure = nil, want error")
			}
			if !strings.Contains(err.Error(), http.StatusText(tc.status)) &&
				!strings.Contains(err.Error(), "status") {
				t.Errorf("error %q should mention the status", err)
			}
			if tc.body != "" && !strings.Contains(err.Error(), tc.body) {
				t.Errorf("error %q should include response body %q", err, tc.body)
			}
		})
	}
}

func TestHTTPEnsureConnectionError(t *testing.T) {
	// Closed server: connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	if err := (&HTTP{Endpoint: srv.URL, Token: "t"}).Ensure(context.Background(), nil); err == nil {
		t.Fatal("Ensure = nil, want connection error")
	}
}

// TestTimeoutOrdering pins the invariant chain across the registration path:
// one reload < server WriteTimeout coverage (2*reload) < client timeout.
// A client timeout at or below 2*ReloadTimeout can expire on a request the
// server will still complete (queued behind one in-flight reload).
func TestTimeoutOrdering(t *testing.T) {
	if DefaultTimeout <= 2*registrar.ReloadTimeout {
		t.Fatalf("DefaultTimeout (%v) must exceed 2*ReloadTimeout (%v)",
			DefaultTimeout, 2*registrar.ReloadTimeout)
	}
}
