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

func TestNodeDNSDefaultsToGatewayPlusFallbacks(t *testing.T) {
	m := fakeGatewayManager(t, inspectWithGateway, false)
	dns := m.nodeDNS(Config{})
	if got := strings.Join(dns.Servers, ","); got != "192.168.64.1,1.1.1.1,8.8.8.8" {
		t.Errorf("servers = %q, want gateway first then public fallbacks", got)
	}
	if len(dns.Options) == 0 {
		t.Error("expected resolv.conf options bounding per-server timeouts")
	}
}

func TestNodeDNSExplicitConfigWins(t *testing.T) {
	m := fakeGatewayManager(t, inspectWithGateway, false)
	dns := m.nodeDNS(Config{DNS: []string{"10.0.0.53"}})
	if got := strings.Join(dns.Servers, ","); got != "10.0.0.53" {
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
