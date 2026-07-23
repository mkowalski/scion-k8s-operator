package bootstrap

import (
	"context"
	"fmt"
	"net"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
)

const discoveryPort = 8041 // default discovery server port, netsec-ethz/bootstrapper

// DHCPDiscoverer is the "dhcp" bootstrap mode: DHCP requesting
// option 72 (WWW server) per the endhost-bootstrap design.
type DHCPDiscoverer struct {
	Interface string // host interface to query on, e.g. br-ex or eno1
}

func urlsFromWWWServers(ips []net.IP) []string {
	urls := make([]string, 0, len(ips))
	for _, ip := range ips {
		urls = append(urls, fmt.Sprintf("http://%s:%d", ip, discoveryPort))
	}
	return urls
}

func (d *DHCPDiscoverer) BaseURLs(ctx context.Context) ([]string, error) {
	c, err := nclient4.New(d.Interface)
	if err != nil {
		return nil, fmt.Errorf("dhcp client on %s: %w", d.Interface, err)
	}
	defer c.Close()
	offer, err := c.DiscoverOffer(ctx,
		dhcpv4.WithRequestedOptions(dhcpv4.OptionDefaultWorldWideWebServer))
	if err != nil {
		return nil, fmt.Errorf("dhcp discover: %w", err)
	}
	ips := dhcpv4.GetIPs(dhcpv4.OptionDefaultWorldWideWebServer, offer.Options)
	if len(ips) == 0 {
		return nil, fmt.Errorf("dhcp: no option 72 (WWW server) in offer")
	}
	return urlsFromWWWServers(ips), nil
}
