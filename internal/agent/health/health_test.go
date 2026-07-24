package health

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func get(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestReadyz(t *testing.T) {
	h := New()
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	if code := get(t, srv.URL+"/readyz"); code != 503 {
		t.Fatalf("not ready yet, want 503 got %d", code)
	}
	h.SetReady("bootstrap", true)
	h.SetReady("gateway", true)
	if code := get(t, srv.URL+"/readyz"); code != 200 {
		t.Fatalf("want 200 got %d", code)
	}
	if code := get(t, srv.URL+"/healthz"); code != 200 {
		t.Fatalf("healthz want 200 got %d", code)
	}
}

func TestMetrics(t *testing.T) {
	h := New()
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("metrics want 200 got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Fatal("metrics body empty")
	}
}
