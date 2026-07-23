package config

import "testing"

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
