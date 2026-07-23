package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestURLDiscovererFetch(t *testing.T) {
	topo := []byte(`{"isd_as": "1-ff00:0:112", "mtu": 1472}`)
	trc := []byte("fake-trc-der-bytes")
	mux := http.NewServeMux()
	mux.HandleFunc("/topology", func(w http.ResponseWriter, r *http.Request) { w.Write(topo) })
	mux.HandleFunc("/trcs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":{"base_number":1,"isd":1,"serial_number":1}}]`))
	})
	mux.HandleFunc("/trcs/isd1-b1-s1/blob", func(w http.ResponseWriter, r *http.Request) { w.Write(trc) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	d := &URLDiscoverer{BaseURL: srv.URL}
	if err := Fetch(context.Background(), d, dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "topology.json"))
	if err != nil || string(got) != string(topo) {
		t.Fatalf("topology not cached: %v %q", err, got)
	}
	if _, err := os.Stat(filepath.Join(dir, "certs", "ISD1-B1-S1.trc")); err != nil {
		t.Fatalf("trc not cached: %v", err)
	}
}

func TestFetchInvalidTopology(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/topology", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("not-json")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := &URLDiscoverer{BaseURL: srv.URL}
	if err := Fetch(context.Background(), d, t.TempDir()); err == nil {
		t.Fatal("expected error for invalid topology")
	}
}
