package bootstrap

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// DNSDiscoverer is the "dns" bootstrap mode: SRV lookup of
// _sciondiscovery._tcp.<domain> per the endhost-bootstrap design
// (doc/dev/design/endhost-bootstrap.rst in scionproto/scion).
type DNSDiscoverer struct {
	Domain    string
	lookupSRV func(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
}

// BaseURLs resolves _sciondiscovery._tcp.<Domain> SRV records and returns
// the discovery server base URLs in resolver order.
func (d *DNSDiscoverer) BaseURLs(ctx context.Context) ([]string, error) {
	lookup := d.lookupSRV
	if lookup == nil {
		lookup = net.DefaultResolver.LookupSRV
	}
	_, srvs, err := lookup(ctx, "sciondiscovery", "tcp", d.Domain)
	if err != nil {
		return nil, fmt.Errorf("SRV _sciondiscovery._tcp.%s: %w", d.Domain, err)
	}
	urls := make([]string, 0, len(srvs))
	for _, s := range srvs {
		urls = append(urls, fmt.Sprintf("http://%s:%d", strings.TrimSuffix(s.Target, "."), s.Port))
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("no SRV records for _sciondiscovery._tcp.%s", d.Domain)
	}
	return urls, nil
}
