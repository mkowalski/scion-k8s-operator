package sig

import (
	"net"
	"testing"
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
