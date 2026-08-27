package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mkowalski/scion-k8s-operator/internal/agent/config"
	"github.com/mkowalski/scion-k8s-operator/internal/agent/kube"
)

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{" a , b ,", []string{"a", "b"}},
		{",,", nil},
	} {
		if got := split(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("split(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestLocalIA(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "topology.json")
	if err := os.WriteFile(path, []byte(`{"isd_as": "1-ff00:0:112"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ia, err := localIA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ia != "1-ff00:0:112" {
		t.Errorf("localIA = %q, want 1-ff00:0:112", ia)
	}
}

func TestLocalIAErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := localIA(dir); err == nil {
		t.Error("expected error for missing topology.json")
	}
	os.WriteFile(filepath.Join(dir, "topology.json"), []byte(`{}`), 0o644)
	if _, err := localIA(dir); err == nil {
		t.Error("expected error for missing isd_as field")
	}
}

func TestAdvertisedNets(t *testing.T) {
	info := kube.NodeInfo{PodCIDRs: []string{"10.128.0.0/23"}, InternalIP: "192.0.2.10"}
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want []string
	}{
		{"both", config.Config{AdvertisePodCIDR: true, AdvertiseNodeIP: true},
			[]string{"10.128.0.0/23", "192.0.2.10/32"}},
		{"podOnly", config.Config{AdvertisePodCIDR: true},
			[]string{"10.128.0.0/23"}},
		{"nodeOnly", config.Config{AdvertiseNodeIP: true},
			[]string{"192.0.2.10/32"}},
		{"neither", config.Config{}, nil},
	} {
		if got := advertisedNets(info, tc.cfg); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: advertisedNets = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAdvertisedNetsIPv4Only(t *testing.T) {
	// The dataplane is IPv4-only: dual-stack pod CIDRs and IPv6 node IPs
	// must not be advertised into SGRP (a remote SIG would install an
	// IPv6 route toward a gateway that cannot serve it).
	info := kube.NodeInfo{
		PodCIDRs:   []string{"10.130.0.0/23", "fd01:0:0:3::/64"},
		InternalIP: "2001:db8::1",
	}
	got := advertisedNets(info, config.Config{AdvertisePodCIDR: true, AdvertiseNodeIP: true})
	want := []string{"10.130.0.0/23"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("advertisedNets = %v, want %v", got, want)
	}
}

func TestNodeInfoLocalPrefixesBypass(t *testing.T) {
	t.Setenv("SCION_LOCAL_PREFIXES", "10.0.0.0/24, 10.0.1.0/24")
	t.Setenv("SCION_NODE_IP", "192.0.2.7")
	info, dynamicIPAM, err := nodeInfo(t.Context(), config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if dynamicIPAM {
		t.Error("dynamicIPAM = true for SCION_LOCAL_PREFIXES bypass")
	}
	if want := []string{"10.0.0.0/24", "10.0.1.0/24"}; !reflect.DeepEqual(info.PodCIDRs, want) {
		t.Errorf("PodCIDRs = %v, want %v", info.PodCIDRs, want)
	}
	if info.InternalIP != "192.0.2.7" {
		t.Errorf("InternalIP = %q, want 192.0.2.7", info.InternalIP)
	}
}

func TestNodeInfoLocalPrefixesInvalidIP(t *testing.T) {
	t.Setenv("SCION_LOCAL_PREFIXES", "10.0.0.0/24")
	t.Setenv("SCION_NODE_IP", "not-an-ip")
	if _, _, err := nodeInfo(t.Context(), config.Config{}); err == nil {
		t.Error("expected error for invalid SCION_NODE_IP")
	}
}

func TestNodeInfoLocalPrefixesMissingIP(t *testing.T) {
	t.Setenv("SCION_LOCAL_PREFIXES", "10.0.0.0/24")
	t.Setenv("SCION_NODE_IP", "")
	if _, _, err := nodeInfo(t.Context(), config.Config{}); err == nil {
		t.Error("expected error when SCION_NODE_IP is unset")
	}
}

// TestCloseOnceDoubleCall guards the OnUp readiness path: even if the
// gateway ever fires OnUp more than once, the sigUp close must not panic.
func TestCloseOnceDoubleCall(t *testing.T) {
	ch := make(chan struct{})
	f := closeOnce(ch)
	f()
	f() // must not panic
	select {
	case <-ch:
	default:
		t.Fatal("channel not closed")
	}
}

func TestCheckAdvertisable(t *testing.T) {
	// No IPv4 pod CIDR discoverable (Calico/Cilium-style IPAM or
	// IPv6-only): must error when advertisement is on, pass when off.
	info := kube.NodeInfo{PodCIDRs: []string{"fd01::/64"}, InternalIP: "192.0.2.1"}
	cfg := config.Config{NodeName: "n1", AdvertisePodCIDR: true}
	nets := advertisedNets(info, cfg)
	if err := checkAdvertisable(cfg, info, nets); err == nil ||
		!strings.Contains(err.Error(), "no IPv4 pod CIDR") {
		t.Fatalf("checkAdvertisable = %v, want no-IPv4-pod-CIDR error", err)
	}
	cfg.AdvertisePodCIDR = false
	if err := checkAdvertisable(cfg, info, advertisedNets(info, cfg)); err != nil {
		t.Fatalf("checkAdvertisable with advertisement off = %v, want nil", err)
	}
	// Healthy case.
	info.PodCIDRs = []string{"10.244.0.0/24"}
	cfg.AdvertisePodCIDR = true
	if err := checkAdvertisable(cfg, info, advertisedNets(info, cfg)); err != nil {
		t.Fatalf("checkAdvertisable healthy = %v, want nil", err)
	}
}
