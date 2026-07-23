// Package policy renders SCION-IP gateway (SIG) prefix-exchange policies:
// the IP routing policy (text format parsed by
// github.com/scionproto/scion/gateway/routing, see marshal.go in that
// package at v0.15.0) and the traffic policy JSON (parsed by
// gateway/control/sessionpolicy.go LegacySessionPolicyAdapter; sample at
// dist/conffiles/gateway.json upstream).
//
// Guardrails: accepted (remote) prefixes must never overlap the cluster,
// service, or machine networks, and a default route is never accepted.
// The routing policy text has no "except" syntax, so "everything except
// the forbidden CIDRs" is computed by subtracting the forbidden CIDRs from
// 0.0.0.0/0 into a minimal covering prefix set.
//
// AdvertisedNets are intentionally NOT validated against ForbiddenCIDRs:
// this node's pod CIDR is inside the cluster network by definition, and the
// advertised nets are ours to announce. Only accepted/remote prefixes are
// guarded.
package policy

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Input describes what a node advertises and accepts.
type Input struct {
	LocalIA        string   // e.g. 1-ff00:0:112
	AdvertisedNets []string // this node's pod CIDR + node IP /32
	AcceptISDASes  []string // remote ASes we exchange prefixes with
	ForbiddenCIDRs []string // clusterNetwork, serviceNetwork, machineNetwork
}

// RenderRoutingPolicy renders the SIG IP routing policy in the text format
// verified against scion v0.15.0 gateway/routing/marshal.go (parseRule):
//
//	<action> <from-ia> <to-ia> <prefix>[,<prefix>...] [# comment]
//
// For each remote IA it emits advertise rules (local -> remote) for every
// advertised net, and accept rules (remote -> local) for the full IPv4
// space minus ForbiddenCIDRs.
//
// The rendered policy relies on the SIG's default-deny behavior: the
// gateway loads routing policies with DefaultAction Reject (scion v0.15.0
// gateway/loader.go:113), so any traffic not matched by an explicit accept
// rule here is rejected; we never need (and never emit) reject rules.
func RenderRoutingPolicy(in Input) (string, error) {
	accepted, err := prepare(in)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, remote := range in.AcceptISDASes {
		for _, net := range in.AdvertisedNets {
			fmt.Fprintf(&b, "advertise %s %s %s\n", in.LocalIA, remote, net)
		}
		fmt.Fprintf(&b, "accept %s %s %s\n", remote, in.LocalIA, strings.Join(accepted, ","))
	}
	return b.String(), nil
}

// trafficPolicyDoc mirrors the JSON shape consumed by scion v0.15.0
// gateway/control/sessionpolicy.go (LegacySessionPolicyAdapter.Parse);
// sample at dist/conffiles/gateway.json.
type trafficPolicyDoc struct {
	ASes map[string]trafficPolicyAS `json:"ASes"`
	// ConfigVersion is parsed but ignored by scion v0.15.0
	// (gateway/control/sessionpolicy.go: cfg.ConfigVersion is never read
	// after unmarshal); reloads are SIGHUP-driven and unconditional, so a
	// constant value is safe today. Re-check this on scion upgrades in
	// case newer versions start comparing versions to skip reloads.
	ConfigVersion uint64 `json:"ConfigVersion"`
}

type trafficPolicyAS struct {
	Nets []string `json:"Nets"`
}

// RenderTrafficPolicy renders the SIG traffic policy JSON mapping each
// remote IA to the allowed (forbidden-subtracted) prefix set.
func RenderTrafficPolicy(in Input) (string, error) {
	accepted, err := prepare(in)
	if err != nil {
		return "", err
	}
	doc := trafficPolicyDoc{
		ASes:          make(map[string]trafficPolicyAS, len(in.AcceptISDASes)),
		ConfigVersion: 1,
	}
	for _, remote := range in.AcceptISDASes {
		doc.ASes[remote] = trafficPolicyAS{Nets: accepted}
	}
	raw, err := json.MarshalIndent(doc, "", "    ")
	if err != nil {
		return "", err
	}
	return string(raw) + "\n", nil
}

// prepare validates the input and computes the accepted prefix set.
func prepare(in Input) ([]string, error) {
	if in.LocalIA == "" {
		return nil, fmt.Errorf("local IA must not be empty")
	}
	if len(in.AcceptISDASes) == 0 {
		return nil, fmt.Errorf("no remote ISD-ASes to exchange prefixes with")
	}
	accepted, err := subtractCIDRs("0.0.0.0/0", in.ForbiddenCIDRs)
	if err != nil {
		return nil, fmt.Errorf("computing accepted prefixes: %w", err)
	}
	// Defense in depth: the subtraction must never yield an overlap or a
	// default route (the latter is possible only when ForbiddenCIDRs is
	// empty, which we reject as unsafe).
	if err := ValidateNoOverlap(accepted, in.ForbiddenCIDRs); err != nil {
		return nil, fmt.Errorf("guardrail violated: %w", err)
	}
	return accepted, nil
}

// ValidateNoOverlap errors if any net overlaps any forbidden CIDR or is a
// default route. Runtime defense-in-depth check for prefixes accepted from
// remote ASes.
func ValidateNoOverlap(nets, forbidden []string) error {
	fps := make([]netip.Prefix, 0, len(forbidden))
	for _, f := range forbidden {
		fp, err := netip.ParsePrefix(f)
		if err != nil {
			return fmt.Errorf("parsing forbidden CIDR %q: %w", f, err)
		}
		fps = append(fps, fp)
	}
	for _, n := range nets {
		p, err := netip.ParsePrefix(n)
		if err != nil {
			return fmt.Errorf("parsing prefix %q: %w", n, err)
		}
		if p.Bits() == 0 {
			return fmt.Errorf("prefix %s is a default route", n)
		}
		for _, fp := range fps {
			if p.Overlaps(fp) {
				return fmt.Errorf("prefix %s overlaps forbidden CIDR %s", n, fp)
			}
		}
	}
	return nil
}

// subtractCIDRs returns a minimal covering prefix set equal to base minus
// excludes, computed by recursive halving. IPv4 only: IPv6 prefixes in base
// or excludes are rejected with an error (the operator currently manages
// IPv4 cluster networks only; supporting IPv6 requires plumbing dual-stack
// through the whole agent, so failing loudly beats silently mis-filtering).
//
// A prefix p is fully excluded by e iff e contains p, i.e.
// e.Contains(p.Addr()) && e.Bits() <= p.Bits(): aligned prefixes either
// nest or are disjoint, so if e (the wider prefix) contains p's base
// address it contains all of p. Correctness is exercised by the coverage
// property test in policy_test.go.
func subtractCIDRs(base string, excludes []string) ([]string, error) {
	b, err := netip.ParsePrefix(base)
	if err != nil {
		return nil, fmt.Errorf("parsing base %q: %w", base, err)
	}
	if !b.Addr().Is4() {
		return nil, fmt.Errorf("base %s: only IPv4 is supported", base)
	}
	b = b.Masked()
	ex := make([]netip.Prefix, 0, len(excludes))
	for _, e := range excludes {
		p, err := netip.ParsePrefix(e)
		if err != nil {
			return nil, fmt.Errorf("parsing exclude %q: %w", e, err)
		}
		if !p.Addr().Is4() {
			return nil, fmt.Errorf("exclude %s: only IPv4 is supported", e)
		}
		ex = append(ex, p.Masked())
	}

	var out []string
	var walk func(p netip.Prefix)
	walk = func(p netip.Prefix) {
		overlap := false
		for _, e := range ex {
			if e.Bits() <= p.Bits() && e.Contains(p.Addr()) {
				return // e covers all of p: fully excluded
			}
			if p.Overlaps(e) {
				overlap = true
			}
		}
		if !overlap {
			out = append(out, p.String())
			return
		}
		if p.Bits() >= 32 {
			return // cannot split further; /32 overlapping an exclude is excluded
		}
		left := netip.PrefixFrom(p.Addr(), p.Bits()+1)
		walk(left)
		walk(netip.PrefixFrom(upperHalfAddr(p), p.Bits()+1))
	}
	walk(b)
	sort.Strings(out)
	return out, nil
}

// upperHalfAddr returns the base address of the upper half of an IPv4
// prefix, i.e. p.Addr() with bit index p.Bits() (0-based from the MSB) set.
func upperHalfAddr(p netip.Prefix) netip.Addr {
	a4 := p.Addr().As4()
	bit := p.Bits() // set the (bit)-th bit, 0-indexed from MSB
	a4[bit/8] |= 1 << (7 - bit%8)
	return netip.AddrFrom4(a4)
}
