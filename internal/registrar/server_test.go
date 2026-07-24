package registrar

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, token string) (*Server, string, *int) {
	t.Helper()
	topo := `{"isd_as":"1-ff00:0:112","sigs":{"old-sig":{"ctrl_addr":"192.0.2.1:30256","data_addr":"192.0.2.1:30056"}}}`
	f := writeTopo(t, topo)
	reloads := 0
	s := &Server{
		TopologyFile: f,
		Prefix:       "k8s-",
		Token:        token,
		Reload:       func() error { reloads++; return nil },
	}
	return s, f, &reloads
}

func doReq(t *testing.T, s *Server, method, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/v1/sigs", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestPutSigsReplacesManagedSet(t *testing.T) {
	s, f, reloads := newTestServer(t, "secret")
	body := `{"worker-0":{"ctrl_addr":"192.0.2.11:30256","data_addr":"192.0.2.11:30056"}}`
	rec := doReq(t, s, http.MethodPut, "secret", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := readSigs(t, f)
	if _, ok := got["k8s-worker-0"]; !ok {
		t.Fatalf("managed sig missing: %v", got)
	}
	if _, ok := got["old-sig"]; !ok {
		t.Fatalf("unmanaged sig must be preserved: %v", got)
	}
	if *reloads != 1 {
		t.Fatalf("reload invoked %d times, want 1", *reloads)
	}
}

func TestAuthRejected(t *testing.T) {
	s, _, reloads := newTestServer(t, "secret")
	for _, token := range []string{"", "wrong"} {
		rec := doReq(t, s, http.MethodPut, token, `{}`)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", token, rec.Code)
		}
	}
	if *reloads != 0 {
		t.Fatalf("reload must not run on rejected requests")
	}
}

func TestEmptyTokenFailsClosed(t *testing.T) {
	s, _, _ := newTestServer(t, "")
	// even an empty bearer token must be rejected when no token configured
	for _, token := range []string{"", "anything"} {
		rec := doReq(t, s, http.MethodGet, token, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401 (fail-closed)", token, rec.Code)
		}
	}
}

func TestMalformedJSON(t *testing.T) {
	s, _, reloads := newTestServer(t, "secret")
	rec := doReq(t, s, http.MethodPut, "secret", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if *reloads != 0 {
		t.Fatalf("reload must not run on malformed input")
	}
}

func TestGetSigsDerivedFromFile(t *testing.T) {
	s, _, _ := newTestServer(t, "secret")
	body := `{"worker-0":{"ctrl_addr":"192.0.2.11:30256","data_addr":"192.0.2.11:30056"}}`
	if rec := doReq(t, s, http.MethodPut, "secret", body); rec.Code != http.StatusOK {
		t.Fatalf("put: status %d", rec.Code)
	}
	rec := doReq(t, s, http.MethodGet, "secret", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status %d", rec.Code)
	}
	var got map[string]SIG
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := SIG{CtrlAddr: "192.0.2.11:30256", DataAddr: "192.0.2.11:30056"}
	if got["worker-0"] != want {
		t.Fatalf("got %v", got)
	}
	if len(got) != 1 {
		t.Fatalf("unmanaged entries must not appear: %v", got)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	s, _, _ := newTestServer(t, "secret")
	rec := doReq(t, s, http.MethodPost, "secret", `{}`)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rec.Code)
	}
}
