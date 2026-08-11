package cluster

import (
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

// Node VMs boot with the vmnet gateway (the macOS host, where
// mDNSResponder serves DNS for NAT guests) as their only nameserver.
// That resolver disappears whenever VPN software assumes control of
// the host's port 53 for its own DNS proxy (typically by rewriting only
// the system's *effective* resolver, State:/Network/Global/DNS — see
// hostPrimaryDNS), and every in-guest image pull then dies with
// "lookup ... connection refused".
//
// The default nameserver list handles that without hardcoding public
// DNS as the primary fix: gateway first (so nothing changes while it
// works), then this Mac's own configured DNS servers discovered
// straight from macOS's network settings (so internal/corporate names
// still resolve), then a public resolver as a guaranteed last resort —
// reachable from virtually any network, which matters because the
// discovered servers are a snapshot taken at boot and can go stale if
// the laptop later joins a different network. resolv.conf honors at
// most 3 nameservers, so the non-backstop entries are capped at 2 to
// always leave the last slot for a backstop.
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
// explicit Config.DNS wins as-is; otherwise the default is built from
// the network gateway and this Mac's own DNS servers (see
// buildDNSServers), degrading to the zero value (the runtime's stock
// resolv.conf) when the gateway cannot be determined, so a cluster
// always boots at least as well as before this feature existed.
func (m *Manager) nodeDNS(cfg Config) nodeDNSConfig {
	if len(cfg.DNS) > 0 {
		return nodeDNSConfig{Servers: cfg.DNS, Options: nodeDNSOptions}
	}
	gw, err := m.rt.NetworkGateway("default")
	if err != nil || gw == "" {
		return nodeDNSConfig{}
	}
	return nodeDNSConfig{Servers: buildDNSServers(gw, hostPrimaryDNS()), Options: nodeDNSOptions}
}

// buildDNSServers orders the gateway first, then at most one
// host-discovered resolver, then public backstops, always leaving at
// least one of the final two slots for a backstop: the discovered
// servers are a snapshot of whatever network this Mac was on when the
// cluster booted, and are no more trustworthy than the gateway once
// that network changes (roaming laptop) or the discovered address
// itself isn't reachable from the guest. A public backstop is the one
// entry that stays valid regardless of which network the host is on.
// Duplicates are dropped; resolv.conf honors at most 3 nameservers.
func buildDNSServers(gateway string, discovered []string) []string {
	servers := []string{gateway}
	for _, d := range discovered {
		if len(servers) >= 2 {
			break
		}
		if d != "" && !slices.Contains(servers, d) {
			servers = append(servers, d)
		}
	}
	for _, fb := range nodeDNSFallbacks {
		if len(servers) >= 3 {
			break
		}
		if !slices.Contains(servers, fb) {
			servers = append(servers, fb)
		}
	}
	return servers
}

// hostPrimaryDNS reads the DNS servers macOS would hand its own stub
// resolver for the network's primary service — i.e. what
// /etc/resolv.conf on the Mac would say if no VPN or DNS-proxy client
// were rewriting the system's effective resolver. Such clients
// typically only override the merged, effective record
// (State:/Network/Global/DNS, what `scutil --dns` and /etc/resolv.conf
// show); the per-service record survives untouched, so this stays
// accurate even while one is connected. A package-level var so tests
// can swap in a fake without shelling out to scutil.
var hostPrimaryDNS = func() []string {
	service := scutilPrimaryService(runScutil("State:/Network/Global/IPv4"))
	if service == "" {
		return nil
	}
	return scutilServerAddresses(runScutil("State:/Network/Service/" + service + "/DNS"))
}

// runScutil runs `scutil show <key>` and returns its output, or ""
// on any failure (no such key, scutil missing, no network). scutil
// has no single-shot "show a key" flag; it only reads commands from
// stdin.
func runScutil(key string) string {
	cmd := exec.Command("scutil")
	cmd.Stdin = strings.NewReader("show " + key + "\n")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

var scutilPrimaryServiceRe = regexp.MustCompile(`(?m)^\s*PrimaryService\s*:\s*(\S+)`)

// scutilPrimaryService extracts PrimaryService from the
// `show State:/Network/Global/IPv4` dictionary, e.g.:
//
//	<dictionary> {
//	  PrimaryInterface : en0
//	  PrimaryService : D9280C95-C4BC-4CF3-BA35-2493DD06548B
//	  Router : 192.168.16.1
//	}
func scutilPrimaryService(out string) string {
	m := scutilPrimaryServiceRe.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1]
}

var scutilArrayEntryRe = regexp.MustCompile(`^\d+\s*:\s*(\S+)$`)

// scutilServerAddresses extracts the ServerAddresses array from a
// `show State:/Network/Service/<id>/DNS` dictionary, e.g.:
//
//	<dictionary> {
//	  SearchDomains : <array> {
//	    0 : lan
//	  }
//	  ServerAddresses : <array> {
//	    0 : 8.8.8.8
//	    1 : 8.8.4.4
//	  }
//	}
//
// Parsing is scoped to lines after the ServerAddresses key so an
// earlier array (SearchDomains) never leaks in; only lines that parse
// as an IP address are kept, both as a sanity check on scutil's output
// format and to skip the numbered SearchDomains-style entries if the
// scope tracking above is ever wrong about where the array ends.
func scutilServerAddresses(out string) []string {
	idx := strings.Index(out, "ServerAddresses")
	if idx < 0 {
		return nil
	}
	var addrs []string
	for _, line := range strings.Split(out[idx:], "\n") {
		line = strings.TrimSpace(line)
		if line == "}" {
			break
		}
		m := scutilArrayEntryRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if net.ParseIP(m[1]) != nil {
			addrs = append(addrs, m[1])
		}
	}
	return addrs
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
