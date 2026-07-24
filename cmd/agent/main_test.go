package main

import (
	"os"
	"path/filepath"
	"reflect"
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

func TestAdvertisedNetsIPv6(t *testing.T) {
	info := kube.NodeInfo{InternalIP: "2001:db8::1"}
	got := advertisedNets(info, config.Config{AdvertiseNodeIP: true})
	want := []string{"2001:db8::1/128"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("advertisedNets = %v, want %v", got, want)
	}
}

func TestNodeInfoLocalPrefixesBypass(t *testing.T) {
	t.Setenv("SCION_LOCAL_PREFIXES", "10.0.0.0/24, 10.0.1.0/24")
	t.Setenv("SCION_NODE_IP", "192.0.2.7")
	info, err := nodeInfo(t.Context(), config.Config{})
	if err != nil {
		t.Fatal(err)
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
	if _, err := nodeInfo(t.Context(), config.Config{}); err == nil {
		t.Error("expected error for invalid SCION_NODE_IP")
	}
}

func TestNodeInfoLocalPrefixesMissingIP(t *testing.T) {
	t.Setenv("SCION_LOCAL_PREFIXES", "10.0.0.0/24")
	t.Setenv("SCION_NODE_IP", "")
	if _, err := nodeInfo(t.Context(), config.Config{}); err == nil {
		t.Error("expected error when SCION_NODE_IP is unset")
	}
}
