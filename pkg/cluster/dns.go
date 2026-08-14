package cluster

import (
	"fmt"
	"net"
)

// Node VMs boot with the vmnet gateway (the macOS host, where
// mDNSResponder serves DNS for NAT guests) as their only nameserver.
// That resolver disappears whenever VPN software assumes control of
// the host's port 53 for its own DNS proxy, and every in-guest image
// pull then dies with "lookup ... connection refused". Public
// fallbacks fix that: the gateway stays first, so nothing changes
// while it works
// (glibc tries nameservers in order, musl asks all and takes the first
// answer), and when it refuses, resolution fails over in milliseconds.
// resolv.conf honors at most 3 nameservers, so gateway + 2 is the cap.
var nodeDNSFallbacks = []string{"1.1.1.1", "8.8.8.8"}

// nodeDNSOptions bounds the stall when a nameserver silently drops
// queries instead of refusing them: 2s per try, 2 tries, then the next
// server — instead of glibc's default 5s x 2 per server.
var nodeDNSOptions = []string{"timeout:2", "attempts:2"}

// nodeDNSConfig is the resolver setup every node VM of a cluster boots
// with; the zero value keeps the runtime's stock resolv.conf.
type nodeDNSConfig struct {
	Servers []string
	Options []string
}

// nodeDNS returns the resolver setup for a cluster's node VMs. An
// explicit Config.DNS wins as-is; otherwise the default is the network
// gateway plus public fallbacks, degrading to the zero value (the
// runtime's stock resolv.conf) when the gateway cannot be determined,
// so a cluster always boots at least as well as before this feature
// existed.
func (m *Manager) nodeDNS(cfg Config) nodeDNSConfig {
	if len(cfg.DNS) > 0 {
		return nodeDNSConfig{Servers: cfg.DNS, Options: nodeDNSOptions}
	}
	gw, err := m.rt.NetworkGateway("default")
	if err != nil || gw == "" {
		return nodeDNSConfig{}
	}
	return nodeDNSConfig{Servers: append([]string{gw}, nodeDNSFallbacks...), Options: nodeDNSOptions}
}

// validateDNS rejects a non-IP --dns value before any VM boots; the
// container CLI wants literal nameserver addresses, not hostnames
// (which would need a resolver to resolve the resolver).
func validateDNS(cfg Config) error {
	for _, s := range cfg.DNS {
		if net.ParseIP(s) == nil {
			return fmt.Errorf("invalid --dns %q: use nameserver IP addresses (e.g. --dns 1.1.1.1)", s)
		}
	}
	return nil
}
