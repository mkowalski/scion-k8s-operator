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

type multiDiscoverer struct{ urls []string }

func (d *multiDiscoverer) BaseURLs(context.Context) ([]string, error) { return d.urls, nil }

func goodServer(t *testing.T, topo []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/topology", func(w http.ResponseWriter, r *http.Request) { w.Write(topo) })
	mux.HandleFunc("/trcs", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":{"base_number":1,"isd":1,"serial_number":1}}]`))
	})
	mux.HandleFunc("/trcs/isd1-b1-s1/blob", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("trc")) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchFallbackToSecondBase(t *testing.T) {
	topo := []byte(`{"isd_as": "1-ff00:0:112"}`)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := goodServer(t, topo)

	dir := t.TempDir()
	d := &multiDiscoverer{urls: []string{bad.URL, good.URL}}
	if err := Fetch(context.Background(), d, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "topology.json")); err != nil {
		t.Fatalf("topology not cached: %v", err)
	}
}

func TestFetchNoURLs(t *testing.T) {
	d := &multiDiscoverer{}
	if err := Fetch(context.Background(), d, t.TempDir()); err == nil {
		t.Fatal("expected error for empty URL list")
	}
}

func TestFetchNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	if err := Fetch(context.Background(), &URLDiscoverer{BaseURL: srv.URL}, t.TempDir()); err == nil {
		t.Fatal("expected error for non-200 response")
	}
}

func TestFetchTrailingSlashBase(t *testing.T) {
	topo := []byte(`{"isd_as": "1-ff00:0:112"}`)
	good := goodServer(t, topo)
	dir := t.TempDir()
	if err := Fetch(context.Background(), &URLDiscoverer{BaseURL: good.URL + "/"}, dir); err != nil {
		t.Fatal(err)
	}
}

func TestFetchNoPartialWritesOnFailedBase(t *testing.T) {
	topo := []byte(`{"isd_as": "1-ff00:0:112"}`)
	// First base serves topology and TRC list but fails on the TRC blob.
	mux := http.NewServeMux()
	mux.HandleFunc("/topology", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"isd_as":"1-ff00:0:999"}`)) })
	mux.HandleFunc("/trcs", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":{"base_number":2,"isd":2,"serial_number":2}},{"id":{"base_number":3,"isd":3,"serial_number":3}}]`))
	})
	// First TRC blob is served, second is missing (404) -> the base fails
	// after having downloaded one TRC.
	mux.HandleFunc("/trcs/isd2-b2-s2/blob", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("trc2")) })
	bad := httptest.NewServer(mux)
	defer bad.Close()
	good := goodServer(t, topo)

	dir := t.TempDir()
	d := &multiDiscoverer{urls: []string{bad.URL, good.URL}}
	if err := Fetch(context.Background(), d, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "certs", "ISD2-B2-S2.trc")); !os.IsNotExist(err) {
		t.Fatalf("TRC from failed base must not be written: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "topology.json"))
	if err != nil || string(got) != string(topo) {
		t.Fatalf("topology from good base expected: %v %q", err, got)
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
