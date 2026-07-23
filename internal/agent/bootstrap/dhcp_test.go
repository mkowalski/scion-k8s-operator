package bootstrap

import (
	"net"
	"testing"
)

func TestURLsFromOption72(t *testing.T) {
	got := urlsFromWWWServers([]net.IP{net.IPv4(192, 0, 2, 10)})
	if len(got) != 1 || got[0] != "http://192.0.2.10:8041" {
		t.Fatalf("got %v", got)
	}
}

func TestNewDiscoverer(t *testing.T) {
	tests := []struct {
		mode    string
		want    string
		wantErr bool
	}{
		{mode: "url", want: "*bootstrap.URLDiscoverer"},
		{mode: "dns", want: "*bootstrap.DNSDiscoverer"},
		{mode: "dhcp", want: "*bootstrap.DHCPDiscoverer"},
		{mode: "mdns", want: "*bootstrap.MDNSDiscoverer"},
		{mode: "bogus", wantErr: true},
	}
	for _, tc := range tests {
		d, err := NewDiscoverer(tc.mode, "http://x", "example.org", "eth0")
		if tc.wantErr {
			if err == nil {
				t.Errorf("mode %q: expected error", tc.mode)
			}
			continue
		}
		if err != nil {
			t.Errorf("mode %q: %v", tc.mode, err)
			continue
		}
		if got := typeName(d); got != tc.want {
			t.Errorf("mode %q: got %s, want %s", tc.mode, got, tc.want)
		}
	}
}

func typeName(v any) string {
	switch v.(type) {
	case *URLDiscoverer:
		return "*bootstrap.URLDiscoverer"
	case *DNSDiscoverer:
		return "*bootstrap.DNSDiscoverer"
	case *DHCPDiscoverer:
		return "*bootstrap.DHCPDiscoverer"
	case *MDNSDiscoverer:
		return "*bootstrap.MDNSDiscoverer"
	default:
		return "unknown"
	}
}
