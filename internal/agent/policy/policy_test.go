package policy

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"github.com/scionproto/scion/gateway/control"
	"github.com/scionproto/scion/gateway/routing"
)

func testInput() Input {
	return Input{
		LocalIA:        "1-ff00:0:112",
		AdvertisedNets: []string{"10.128.2.0/23", "192.168.111.20/32"},
		AcceptISDASes:  []string{"1-ff00:0:110", "2-ff00:0:210"},
		ForbiddenCIDRs: []string{"10.128.0.0/14", "172.30.0.0/16", "192.168.111.0/24"},
	}
}

func TestRenderRoutingPolicyContent(t *testing.T) {
	out, err := RenderRoutingPolicy(testInput())
	if err != nil {
		t.Fatalf("RenderRoutingPolicy: %v", err)
	}
	// Verified line format (scion v0.15.1 gateway/routing/marshal.go:72-125):
	//   action from to network[,network...] [# comment]
	for _, remote := range []string{"1-ff00:0:110", "2-ff00:0:210"} {
		for _, net := range []string{"10.128.2.0/23", "192.168.111.20/32"} {
			want := "advertise 1-ff00:0:112 " + remote + " " + net
			if !containsLineWithPrefix(out, want) {
				t.Errorf("missing advertise line %q in:\n%s", want, out)
			}
		}
		if !containsLineWithPrefix(out, "accept "+remote+" 1-ff00:0:112 ") {
			t.Errorf("missing accept line for %s in:\n%s", remote, out)
		}
	}
	// No accepted prefix may overlap a forbidden CIDR or be a default route.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "accept" {
			continue
		}
		for _, s := range strings.Split(fields[3], ",") {
			p := netip.MustParsePrefix(s)
			if p.Bits() == 0 {
				t.Errorf("accept rule contains default route: %s", line)
			}
			for _, f := range testInput().ForbiddenCIDRs {
				if p.Overlaps(netip.MustParsePrefix(f)) {
					t.Errorf("accept prefix %s overlaps forbidden %s", s, f)
				}
			}
		}
	}
}

func containsLineWithPrefix(out, prefix string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return true
		}
	}
	return false
}

// Round-trip: output must parse with the real upstream parser.
func TestRoutingPolicyRoundTrip(t *testing.T) {
	out, err := RenderRoutingPolicy(testInput())
	if err != nil {
		t.Fatalf("RenderRoutingPolicy: %v", err)
	}
	var p routing.Policy
	if err := p.UnmarshalText([]byte(out)); err != nil {
		t.Fatalf("upstream routing.Policy.UnmarshalText rejected output: %v\n%s", err, out)
	}
	if len(p.Rules) == 0 {
		t.Fatal("no rules parsed")
	}
}

// Round-trip traffic policy through the real upstream JSON parser
// (gateway/control/sessionpolicy.go LegacySessionPolicyAdapter).
func TestTrafficPolicyRoundTrip(t *testing.T) {
	out, err := RenderTrafficPolicy(testInput())
	if err != nil {
		t.Fatalf("RenderTrafficPolicy: %v", err)
	}
	pols, err := control.LegacySessionPolicyAdapter{}.Parse(context.Background(), []byte(out))
	if err != nil {
		t.Fatalf("upstream parser rejected traffic policy: %v\n%s", err, out)
	}
	if got := len(pols.RemoteIAs()); got != 2 {
		t.Fatalf("expected 2 remote IAs, got %d", got)
	}
}

func TestTrafficPolicyShape(t *testing.T) {
	out, err := RenderTrafficPolicy(testInput())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ASes map[string]struct {
			Nets []string
		}
		ConfigVersion uint64
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if doc.ConfigVersion == 0 {
		t.Error("ConfigVersion must be non-zero")
	}
	for ia, e := range doc.ASes {
		// Nets must be present but empty: session prefixes come from
		// SGRP discovery filtered by the routing policy; static Nets
		// would be programmed as kernel routes (see RenderTrafficPolicy).
		if e.Nets == nil {
			t.Errorf("Nets missing for %s", ia)
		}
		if len(e.Nets) != 0 {
			t.Errorf("Nets must be empty for %s, got %v", ia, e.Nets)
		}
	}
}

func TestValidateNoOverlap(t *testing.T) {
	forbidden := []string{"10.128.0.0/14", "172.30.0.0/16"}
	cases := []struct {
		name    string
		nets    []string
		wantErr bool
	}{
		{"non-overlapping", []string{"10.200.0.0/16", "192.168.0.0/24"}, false},
		{"overlaps cluster", []string{"10.130.0.0/24"}, true},
		{"contains forbidden", []string{"10.0.0.0/8"}, true},
		{"default route", []string{"0.0.0.0/0"}, true},
		{"invalid cidr", []string{"not-a-cidr"}, true},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNoOverlap(tc.nets, forbidden)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateNoOverlap(%v) = %v, wantErr=%v", tc.nets, err, tc.wantErr)
			}
		})
	}
}

// Property test: subtracting excludes from 0.0.0.0/0 yields prefixes that
// (a) never overlap any exclude and (b) together with the excludes cover
// the full IPv4 space (checked on sample IPs).
func TestSubtractCIDRsProperties(t *testing.T) {
	excludes := []string{"10.128.0.0/14", "172.30.0.0/16", "192.168.111.0/24"}
	out, err := subtractCIDRs("0.0.0.0/0", excludes)
	if err != nil {
		t.Fatal(err)
	}
	exPrefixes := make([]netip.Prefix, len(excludes))
	for i, e := range excludes {
		exPrefixes[i] = netip.MustParsePrefix(e)
	}
	for _, s := range out {
		p := netip.MustParsePrefix(s)
		if p.Bits() == 0 {
			t.Errorf("output contains default route %s", s)
		}
		for _, e := range exPrefixes {
			if p.Overlaps(e) {
				t.Errorf("output %s overlaps exclude %s", p, e)
			}
		}
	}
	samples := []string{
		"0.0.0.0", "1.2.3.4", "9.255.255.255", "10.0.0.0", "10.127.255.255",
		"10.128.0.0", "10.129.1.1", "10.131.255.255", "10.132.0.0",
		"172.29.255.255", "172.30.0.1", "172.30.255.255", "172.31.0.0",
		"192.168.110.255", "192.168.111.0", "192.168.111.128", "192.168.112.0",
		"255.255.255.255",
	}
	for _, s := range samples {
		ip := netip.MustParseAddr(s)
		inExclude := false
		for _, e := range exPrefixes {
			if e.Contains(ip) {
				inExclude = true
			}
		}
		inOutput := false
		for _, o := range out {
			if netip.MustParsePrefix(o).Contains(ip) {
				inOutput = true
			}
		}
		if inExclude && inOutput {
			t.Errorf("ip %s is in both excludes and output", ip)
		}
		if !inExclude && !inOutput {
			t.Errorf("ip %s covered by neither excludes nor output", ip)
		}
	}
}

func TestSubtractCIDRsNoExcludes(t *testing.T) {
	out, err := subtractCIDRs("0.0.0.0/0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0] != "0.0.0.0/0" {
		t.Fatalf("expected base unchanged, got %v", out)
	}
}

func TestRenderRejectsIPv6Forbidden(t *testing.T) {
	in := testInput()
	in.ForbiddenCIDRs = append(in.ForbiddenCIDRs, "fd00::/8")
	if _, err := RenderRoutingPolicy(in); err == nil {
		t.Fatal("expected error for IPv6 forbidden CIDR")
	}
}

func TestRenderRoutingPolicyErrors(t *testing.T) {
	in := testInput()
	in.AcceptISDASes = nil
	if _, err := RenderRoutingPolicy(in); err == nil {
		t.Fatal("expected error with no remote IAs")
	}
	in = testInput()
	in.ForbiddenCIDRs = []string{"bogus"}
	if _, err := RenderRoutingPolicy(in); err == nil {
		t.Fatal("expected error with invalid forbidden CIDR")
	}
}

// Empty ForbiddenCIDRs would accept 0.0.0.0/0 (a default route), which the
// guardrail refuses. Rendering must fail safe-by-default in both renderers.
func TestRenderRejectsEmptyForbiddenCIDRs(t *testing.T) {
	in := testInput()
	in.ForbiddenCIDRs = nil
	if _, err := RenderRoutingPolicy(in); err == nil {
		t.Fatal("RenderRoutingPolicy: expected error with empty ForbiddenCIDRs")
	}
	if _, err := RenderTrafficPolicy(in); err == nil {
		t.Fatal("RenderTrafficPolicy: expected error with empty ForbiddenCIDRs")
	}
}
