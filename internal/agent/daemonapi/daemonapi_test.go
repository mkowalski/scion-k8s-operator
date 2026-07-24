package daemonapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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
