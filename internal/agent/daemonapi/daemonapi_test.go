package daemonapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestRunMissingTopology(t *testing.T) {
	dir := t.TempDir()
	err := Run(context.Background(), dir, dir, "127.0.0.1:0")
	if err == nil {
		t.Fatal("want error for missing topology.json")
	}
}

// TestRunSmoke starts Run with a minimal fabricated topology and expects it
// to come up and shut down cleanly on context cancellation.
func TestRunSmoke(t *testing.T) {
	configDir := t.TempDir()
	stateDir := t.TempDir()
	topo := `{
		"isd_as": "1-ff00:0:110",
		"mtu": 1400,
		"attributes": [],
		"control_service": {
			"cs-1": {"addr": "127.0.0.1:31000"}
		},
		"discovery_service": {
			"cs-1": {"addr": "127.0.0.1:31000"}
		},
		"border_routers": {}
	}`
	if err := os.WriteFile(filepath.Join(configDir, "topology.json"), []byte(topo), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(configDir, "certs"), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, configDir, stateDir, "127.0.0.1:0") }()

	// Give it a moment to start, then shut down.
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not shut down after cancel")
	}
}

// TestRegisterOrReuse covers the duplicate-registration tolerance: the
// second registration of an equivalent collector must not fail and must
// hand back the collector that was registered first, so both callers
// increment the same time series.
func TestRegisterOrReuse(t *testing.T) {
	newVec := func() *prometheus.CounterVec {
		return prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "test_queries_total", Help: "h"},
			[]string{"op"},
		)
	}
	reg := prometheus.NewRegistry()

	first, err := registerOrReuse(reg, newVec())
	if err != nil {
		t.Fatalf("first registration: %v", err)
	}
	second, err := registerOrReuse(reg, newVec())
	if err != nil {
		t.Fatalf("duplicate registration: %v", err)
	}
	if first != second {
		t.Fatal("duplicate registration did not reuse the existing collector")
	}

	// A genuinely conflicting collector (same name, different labels) is
	// not an AlreadyRegisteredError and must surface as an error.
	conflicting := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_queries_total", Help: "h"},
		[]string{"other"},
	)
	if _, err := registerOrReuse(reg, conflicting); err == nil {
		t.Fatal("conflicting collector registration must fail")
	}
}
