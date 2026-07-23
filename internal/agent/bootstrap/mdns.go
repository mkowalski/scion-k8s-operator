package bootstrap

import (
	"context"
	"fmt"
	"time"

	"github.com/grandcat/zeroconf"
)

// MDNSDiscoverer is the "mdns" bootstrap mode: browse for
// _sciondiscovery._tcp via multicast DNS.
type MDNSDiscoverer struct{}

func (d *MDNSDiscoverer) BaseURLs(ctx context.Context) ([]string, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, err
	}
	entries := make(chan *zeroconf.ServiceEntry, 8)
	browseCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := resolver.Browse(browseCtx, "_sciondiscovery._tcp", "local.", entries); err != nil {
		return nil, err
	}
	var urls []string
	for e := range entries {
		for _, ip := range e.AddrIPv4 {
			urls = append(urls, fmt.Sprintf("http://%s:%d", ip, e.Port))
		}
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("mdns: no _sciondiscovery._tcp services found")
	}
	return urls, nil
}
