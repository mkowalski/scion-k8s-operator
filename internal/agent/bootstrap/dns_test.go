package bootstrap

import (
	"context"
	"net"
	"testing"
)

func TestDNSDiscoverer(t *testing.T) {
	d := &DNSDiscoverer{
		Domain: "example.org",
		lookupSRV: func(ctx context.Context, service, proto, name string) (string, []*net.SRV, error) {
			if service != "sciondiscovery" || proto != "tcp" || name != "example.org" {
				t.Fatalf("unexpected query: %s %s %s", service, proto, name)
			}
			return "", []*net.SRV{{Target: "ds.example.org.", Port: 8041}}, nil
		},
	}
	urls, err := d.BaseURLs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 1 || urls[0] != "http://ds.example.org:8041" {
		t.Fatalf("got %v", urls)
	}
}

func TestDNSDiscovererNoRecords(t *testing.T) {
	d := &DNSDiscoverer{
		Domain: "example.org",
		lookupSRV: func(ctx context.Context, service, proto, name string) (string, []*net.SRV, error) {
			return "", nil, nil
		},
	}
	if _, err := d.BaseURLs(context.Background()); err == nil {
		t.Fatal("expected error for empty SRV result")
	}
}

func TestDNSDiscovererRootTarget(t *testing.T) {
	d := &DNSDiscoverer{
		Domain: "example.org",
		lookupSRV: func(ctx context.Context, service, proto, name string) (string, []*net.SRV, error) {
			return "", []*net.SRV{{Target: ".", Port: 0}}, nil
		},
	}
	if _, err := d.BaseURLs(context.Background()); err == nil {
		t.Fatal("expected error for '.' SRV target (RFC 2782: service not available)")
	}
}
