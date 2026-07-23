package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunRefreshes(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/topology", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"isd_as":"1-ff00:0:112"}`))
	})
	mux.HandleFunc("/trcs", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	changed := make(chan struct{}, 16)
	_ = Run(ctx, &URLDiscoverer{BaseURL: srv.URL}, t.TempDir(), 50*time.Millisecond, changed)
	if hits.Load() < 2 {
		t.Fatalf("expected at least 2 fetches, got %d", hits.Load())
	}
	select {
	case <-changed:
	default:
		t.Fatal("expected change notification after first fetch")
	}
}

func TestFetchRejectsPinnedTRCChange(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/topology", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"isd_as":"1-ff00:0:112"}`))
	})
	mux.HandleFunc("/trcs", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":{"isd":1,"base_number":1,"serial_number":1}}]`))
	})
	mux.HandleFunc("/trcs/isd1-b1-s1/blob", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("new-trc-bytes"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	root := t.TempDir()
	dir := filepath.Join(root, "cfg")
	if err := os.MkdirAll(filepath.Join(root, "pinned-trcs"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pin bytes differ from what the server serves; no cached copy exists,
	// so the pin file content itself must be the source of truth.
	if err := os.WriteFile(filepath.Join(root, "pinned-trcs", "ISD1-B1-S1.trc"), []byte("pinned-trc-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Fetch(context.Background(), &URLDiscoverer{BaseURL: srv.URL}, dir)
	if err == nil {
		t.Fatal("expected Fetch to fail for pinned TRC change")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "certs", "ISD1-B1-S1.trc")); !os.IsNotExist(statErr) {
		t.Fatalf("TRC should not be written on pin violation, stat err: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "topology.json")); !os.IsNotExist(statErr) {
		t.Fatalf("topology.json should not be written on pin violation, stat err: %v", statErr)
	}
}

func TestFetchAcceptsMatchingPinnedTRC(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/topology", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"isd_as":"1-ff00:0:112"}`))
	})
	mux.HandleFunc("/trcs", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":{"isd":1,"base_number":1,"serial_number":1}}]`))
	})
	mux.HandleFunc("/trcs/isd1-b1-s1/blob", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("pinned-trc-bytes"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	root := t.TempDir()
	dir := filepath.Join(root, "cfg")
	if err := os.MkdirAll(filepath.Join(root, "pinned-trcs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pinned-trcs", "ISD1-B1-S1.trc"), []byte("pinned-trc-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Fetch(context.Background(), &URLDiscoverer{BaseURL: srv.URL}, dir); err != nil {
		t.Fatalf("Fetch failed despite matching pin: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "certs", "ISD1-B1-S1.trc"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pinned-trc-bytes" {
		t.Fatalf("unexpected TRC content: %q", got)
	}
}
