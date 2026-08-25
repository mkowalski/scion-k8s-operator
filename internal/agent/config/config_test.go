package config

import (
	"testing"
	"time"
)

func TestFromEnv(t *testing.T) {
	t.Setenv("NODE_NAME", "worker-0")
	t.Setenv("SCION_BOOTSTRAP_MODE", "url")
	t.Setenv("SCION_DISCOVERY_URL", "http://as-infra:8041")
	t.Setenv("SCION_TUN_NAME", "scion0")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.NodeName != "worker-0" || c.DiscoveryURL != "http://as-infra:8041" || c.TunName != "scion0" {
		t.Fatalf("bad config: %+v", c)
	}
}

func TestFromEnvMissingNode(t *testing.T) {
	t.Setenv("NODE_NAME", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for missing NODE_NAME")
	}
}

func TestFromEnvTable(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr bool
		check   func(t *testing.T, c Config)
	}{
		{
			name: "defaults",
			env: map[string]string{
				"NODE_NAME":           "worker-0",
				"SCION_DISCOVERY_URL": "http://as-infra:8041",
			},
			check: func(t *testing.T, c Config) {
				if c.BootstrapMode != "url" {
					t.Errorf("BootstrapMode = %q, want url", c.BootstrapMode)
				}
				if c.StateDir != "/var/lib/scion-node-agent" {
					t.Errorf("StateDir = %q", c.StateDir)
				}
				if c.MetricsAddr != ":9465" {
					t.Errorf("MetricsAddr = %q", c.MetricsAddr)
				}
				if c.TunName != "scion0" {
					t.Errorf("TunName = %q", c.TunName)
				}
				if c.RefreshInterval != time.Hour {
					t.Errorf("RefreshInterval = %v, want 1h", c.RefreshInterval)
				}
				// nodeIP advertisement defaults off (fail-safe: an
				// underlay-sharing node IP creates a routing loop).
				if !c.AdvertisePodCIDR || c.AdvertiseNodeIP || !c.EnableDaemonAPI {
					t.Errorf("bool defaults: %+v", c)
				}
			},
		},
		{
			name: "mode url without discovery URL",
			env: map[string]string{
				"NODE_NAME": "worker-0",
			},
			wantErr: true,
		},
		{
			name: "invalid refresh interval",
			env: map[string]string{
				"NODE_NAME":              "worker-0",
				"SCION_DISCOVERY_URL":    "http://as-infra:8041",
				"SCION_REFRESH_INTERVAL": "notaduration",
			},
			wantErr: true,
		},
		{
			name: "invalid bootstrap mode",
			env: map[string]string{
				"NODE_NAME":            "worker-0",
				"SCION_BOOTSTRAP_MODE": "carrier-pigeon",
			},
			wantErr: true,
		},
		{
			name: "mode dns needs no discovery URL",
			env: map[string]string{
				"NODE_NAME":            "worker-0",
				"SCION_BOOTSTRAP_MODE": "dns",
			},
			check: func(t *testing.T, c Config) {
				if c.BootstrapMode != "dns" {
					t.Errorf("BootstrapMode = %q, want dns", c.BootstrapMode)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv registers cleanup; unset vars stay unset.
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c, err := FromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got config %+v", c)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.check != nil {
				tc.check(t, c)
			}
		})
	}
}
