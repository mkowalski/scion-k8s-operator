package sig

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAddrs(t *testing.T) {
	ip := net.ParseIP("192.0.2.10")
	ctrl, data, probe := addrs(ip)

	if !ctrl.IP.Equal(ip) || !data.IP.Equal(ip) || !probe.IP.Equal(ip) {
		t.Fatalf("expected all addrs to use node IP %v, got ctrl=%v data=%v probe=%v",
			ip, ctrl.IP, data.IP, probe.IP)
	}
	if ctrl.Port != 30256 {
		t.Errorf("ctrl port = %d, want 30256", ctrl.Port)
	}
	if data.Port != 30056 {
		t.Errorf("data port = %d, want 30056", data.Port)
	}
	if probe.Port != 30856 {
		t.Errorf("probe port = %d, want 30856", probe.Port)
	}
}

func TestAddrsIPv6(t *testing.T) {
	ip := net.ParseIP("2001:db8::1")
	ctrl, data, probe := addrs(ip)
	for name, a := range map[string]*net.UDPAddr{"ctrl": ctrl, "data": data, "probe": probe} {
		if !a.IP.Equal(ip) {
			t.Errorf("%s addr IP = %v, want %v", name, a.IP, ip)
		}
	}
}

func TestRunNilReloadTrigger(t *testing.T) {
	err := Run(context.Background(), Params{ConfigDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected error for nil ReloadTrigger, got nil")
	}
	// Note: don't just match "ReloadTrigger" — t.TempDir() embeds the test
	// name in file paths, so unrelated file-not-found errors would match.
	if !strings.Contains(err.Error(), "ReloadTrigger must not be nil") {
		t.Errorf("error %q does not mention nil ReloadTrigger guard", err)
	}
}

func TestRunOnUpNotCalledOnConnectorFailure(t *testing.T) {
	called := false
	err := Run(context.Background(), Params{
		ConfigDir:     t.TempDir(), // no topology.json → connector setup fails
		ReloadTrigger: make(chan struct{}, 1),
		OnUp:          func() { called = true },
	})
	if err == nil {
		t.Fatal("expected error for missing topology.json")
	}
	if called {
		t.Error("OnUp was called despite connector failure")
	}
}

func TestRunNilOnUpTolerated(t *testing.T) {
	// nil OnUp must not panic; the run still fails early on the missing
	// topology, before OnUp would fire.
	err := Run(context.Background(), Params{
		ConfigDir:     t.TempDir(),
		ReloadTrigger: make(chan struct{}, 1),
	})
	if err == nil {
		t.Fatal("expected error for missing topology.json")
	}
}

func TestEnableTunForwarding(t *testing.T) {
	t.Run("success once file appears", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "forwarding")
		if err := os.WriteFile(path, []byte("0"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := enableTunForwarding(context.Background(), path, 3, time.Millisecond); err != nil {
			t.Fatalf("enableTunForwarding: %v", err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "1" {
			t.Fatalf("forwarding = %q, want 1", b)
		}
	})

	t.Run("exhausts attempts on missing device", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "no-such-dir", "forwarding")
		err := enableTunForwarding(context.Background(), path, 2, time.Millisecond)
		if err == nil {
			t.Fatal("want error after exhausting attempts")
		}
	})

	t.Run("returns on context cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := enableTunForwarding(ctx, "/nonexistent", 1000, time.Hour)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}
