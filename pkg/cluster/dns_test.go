package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

// fakeGatewayManager backs nodeDNS with a container CLI whose
// `network inspect default` reports the given gateway JSON (or fails
// when the script exits non-zero).
func fakeGatewayManager(t *testing.T, inspectJSON string, fail bool) *Manager {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "container")
	script := `#!/bin/sh
if [ "$1" = "network" ] && [ "$2" = "inspect" ]; then
  if [ "$KIAC_TEST_FAIL" = true ]; then exit 1; fi
  printf '%s' "$KIAC_TEST_INSPECT"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIAC_TEST_INSPECT", inspectJSON)
	if fail {
		t.Setenv("KIAC_TEST_FAIL", "true")
	} else {
		t.Setenv("KIAC_TEST_FAIL", "false")
	}
	return &Manager{rt: &runtime.Client{Bin: bin}}
}

const inspectWithGateway = `[{"id":"default","status":{"ipv4Gateway":"192.168.64.1","ipv4Subnet":"192.168.64.0/24"}}]`

// stubHostPrimaryDNS swaps the package-level hostPrimaryDNS var so
// tests never shell out to the real scutil and never depend on
// whatever network the machine running the test happens to be on.
func stubHostPrimaryDNS(t *testing.T, servers []string) {
	t.Helper()
	orig := hostPrimaryDNS
	hostPrimaryDNS = func() []string { return servers }
	t.Cleanup(func() { hostPrimaryDNS = orig })
}

func TestNodeDNSDefaultsToGatewayThenHostDNSThenBackstop(t *testing.T) {
	stubHostPrimaryDNS(t, []string{"10.0.0.53", "10.0.0.54"})
	m := fakeGatewayManager(t, inspectWithGateway, false)
	dns := m.nodeDNS(Config{})
	if got := strings.Join(dns.Servers, ","); got != "192.168.64.1,10.0.0.53,1.1.1.1" {
		t.Errorf("servers = %q, want gateway, one discovered server, one guaranteed backstop", got)
	}
	if len(dns.Options) == 0 {
		t.Error("expected resolv.conf options bounding per-server timeouts")
	}
}

func TestNodeDNSFallsBackToBackstopsWithNoHostDNSDiscovered(t *testing.T) {
	stubHostPrimaryDNS(t, nil)
	m := fakeGatewayManager(t, inspectWithGateway, false)
	dns := m.nodeDNS(Config{})
	if got := strings.Join(dns.Servers, ","); got != "192.168.64.1,1.1.1.1,8.8.8.8" {
		t.Errorf("servers = %q, want gateway then both public backstops", got)
	}
}

func TestNodeDNSExplicitConfigWins(t *testing.T) {
	stubHostPrimaryDNS(t, []string{"10.0.0.53"})
	m := fakeGatewayManager(t, inspectWithGateway, false)
	dns := m.nodeDNS(Config{DNS: []string{"10.9.9.9"}})
	if got := strings.Join(dns.Servers, ","); got != "10.9.9.9" {
		t.Errorf("servers = %q, want the explicit list untouched", got)
	}
}

func TestNodeDNSDegradesWhenGatewayUnknown(t *testing.T) {
	for name, m := range map[string]*Manager{
		"inspect fails":       fakeGatewayManager(t, "", true),
		"no gateway reported": fakeGatewayManager(t, `[{"id":"default","status":{}}]`, false),
	} {
		dns := m.nodeDNS(Config{})
		if len(dns.Servers) != 0 || len(dns.Options) != 0 {
			t.Errorf("%s: dns = %+v, want zero value (stock resolv.conf)", name, dns)
		}
	}
}

func TestBuildDNSServers(t *testing.T) {
	const gw = "192.168.64.1"
	for _, tc := range []struct {
		name       string
		discovered []string
		want       string
	}{
		{"no discovered servers: both backstops fill the rest", nil, "192.168.64.1,1.1.1.1,8.8.8.8"},
		{"one discovered server: exactly one backstop slot left", []string{"10.0.0.53"}, "192.168.64.1,10.0.0.53,1.1.1.1"},
		{"two discovered servers: only the first is used", []string{"10.0.0.53", "10.0.0.54"}, "192.168.64.1,10.0.0.53,1.1.1.1"},
		{"discovered duplicates the gateway: falls through to the next", []string{gw, "10.0.0.54"}, "192.168.64.1,10.0.0.54,1.1.1.1"},
		{"discovered duplicates a backstop: the other backstop still gets a slot", []string{"1.1.1.1"}, "192.168.64.1,1.1.1.1,8.8.8.8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(buildDNSServers(gw, tc.discovered), ",")
			if got != tc.want {
				t.Errorf("buildDNSServers(%q, %v) = %q, want %q", gw, tc.discovered, got, tc.want)
			}
		})
	}
}

func TestBuildDNSServersAlwaysReservesABackstopSlot(t *testing.T) {
	// Whatever the discovered list looks like, at least one of the
	// three slots must be a known-reachable public backstop -- that
	// is the whole point of capping the non-backstop entries at 2.
	for _, discovered := range [][]string{
		nil,
		{"10.0.0.53"},
		{"10.0.0.53", "10.0.0.54", "10.0.0.55"},
		{"192.168.64.1"}, // duplicates the gateway
		{"1.1.1.1", "8.8.8.8"},
	} {
		servers := buildDNSServers("192.168.64.1", discovered)
		hasBackstop := false
		for _, fb := range nodeDNSFallbacks {
			if slicesContains(servers, fb) {
				hasBackstop = true
			}
		}
		if !hasBackstop {
			t.Errorf("buildDNSServers(gateway, %v) = %v, no backstop present", discovered, servers)
		}
		if len(servers) > 3 {
			t.Errorf("buildDNSServers(gateway, %v) = %v, exceeds resolv.conf's 3-server limit", discovered, servers)
		}
	}
}

func slicesContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestScutilPrimaryService(t *testing.T) {
	const out = `<dictionary> {
  PrimaryInterface : en0
  PrimaryService : D9280C95-C4BC-4CF3-BA35-2493DD06548B
  Router : 192.168.16.1
}
`
	if got := scutilPrimaryService(out); got != "D9280C95-C4BC-4CF3-BA35-2493DD06548B" {
		t.Errorf("scutilPrimaryService = %q", got)
	}
	if got := scutilPrimaryService("<dictionary> {\n}\n"); got != "" {
		t.Errorf("scutilPrimaryService of an empty dictionary = %q, want empty", got)
	}
	if got := scutilPrimaryService(""); got != "" {
		t.Errorf("scutilPrimaryService of empty input = %q, want empty", got)
	}
}

func TestScutilServerAddresses(t *testing.T) {
	const out = `<dictionary> {
  SearchDomains : <array> {
    0 : lan
  }
  ServerAddresses : <array> {
    0 : 8.8.8.8
    1 : 8.8.4.4
  }
}
`
	got := scutilServerAddresses(out)
	if strings.Join(got, ",") != "8.8.8.8,8.8.4.4" {
		t.Errorf("scutilServerAddresses = %v, want [8.8.8.8 8.8.4.4]", got)
	}
	// SearchDomains' own numbered array entries must never leak in,
	// and a non-IP entry (defensive against a format we don't expect)
	// must be dropped rather than handed to the guest as a nameserver.
	const withJunk = `<dictionary> {
  ServerAddresses : <array> {
    0 : not-an-ip
    1 : 9.9.9.9
  }
}
`
	if got := scutilServerAddresses(withJunk); strings.Join(got, ",") != "9.9.9.9" {
		t.Errorf("scutilServerAddresses = %v, want only the valid IP", got)
	}
	if got := scutilServerAddresses("<dictionary> {\n}\n"); len(got) != 0 {
		t.Errorf("scutilServerAddresses of a key with no DNS = %v, want none", got)
	}
	if got := scutilServerAddresses(""); len(got) != 0 {
		t.Errorf("scutilServerAddresses of empty input = %v, want none", got)
	}
}

func TestValidateDNS(t *testing.T) {
	if err := validateDNS(Config{DNS: []string{"1.1.1.1", "fd00::53"}}); err != nil {
		t.Errorf("valid IPs rejected: %v", err)
	}
	if err := validateDNS(Config{}); err != nil {
		t.Errorf("empty DNS rejected: %v", err)
	}
	err := validateDNS(Config{DNS: []string{"dns.example.com"}})
	if err == nil || !strings.Contains(err.Error(), "dns.example.com") {
		t.Errorf("hostname accepted or unclear error: %v", err)
	}
}
