package networkmanager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tui-tools/tui-network/internal/network"
)

// fixture reads a captured nmcli output. The fixtures come from NetworkManager
// running under systemd in Fedora 44 (1.56) and Ubuntu 24.04 (1.46)
// containers, on a veth device with a static profile, a dummy uplink, the
// loopback and an unmanaged veth peer.
func fixture(t *testing.T, distro, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", distro, name+".txt")) //nolint:gosec // fixed test fixture names
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestParseDeviceShowFixtures(t *testing.T) {
	for _, distro := range []string{"fedora", "ubuntu"} {
		t.Run(distro, func(t *testing.T) {
			devices := ParseDeviceShow(fixture(t, distro, "device-show"))
			if len(devices) != 4 {
				t.Fatalf("got %d devices, want 4", len(devices))
			}
			byName := map[string]Device{}
			for _, d := range devices {
				byName[d.Name] = d
			}
			eth := byName["eth1"]
			if eth.StateCode != 100 || eth.State != "connected" {
				t.Errorf("eth1 state = %d %q", eth.StateCode, eth.State)
			}
			if eth.Connection != "lab-eth1" || checkUUID(eth.ConnectionUUID) != nil {
				t.Errorf("eth1 profile = %q %q", eth.Connection, eth.ConnectionUUID)
			}
			if len(eth.IP4Addresses) != 1 || eth.IP4Addresses[0] != "192.0.2.10/24" {
				t.Errorf("eth1 addresses = %v", eth.IP4Addresses)
			}
			if eth.IP4Gateway != "192.0.2.1" || len(eth.DNS) != 1 || eth.DNS[0] != "192.0.2.53" {
				t.Errorf("eth1 gateway/dns = %q %v", eth.IP4Gateway, eth.DNS)
			}
			// The search list is IP4.SEARCHES on both releases captured.
			if len(eth.Searches) != 1 {
				t.Errorf("eth1 searches = %v", eth.Searches)
			}
			lo := byName["lo"]
			if !lo.External() || len(lo.IP6Addresses) != 1 || lo.IP6Addresses[0] != "::1/128" {
				t.Errorf("lo = %+v", lo)
			}
			if peer := byName["peer1"]; peer.StateCode != stateUnmanaged {
				t.Errorf("peer1 state = %d", peer.StateCode)
			}
		})
	}
}

func TestBuildModelFromFixtures(t *testing.T) {
	for _, distro := range []string{"fedora", "ubuntu"} {
		t.Run(distro, func(t *testing.T) {
			devices := ParseDeviceShow(fixture(t, distro, "device-show"))
			profiles := ParseConnectionList(fixture(t, distro, "connection-list"))
			for i := range profiles {
				if profiles[i].Name == "lab-eth1" {
					profiles[i].Settings = ParseProfileSettings(fixture(t, distro, "connection-eth1"))
				}
			}
			model := BuildModel(devices, profiles, map[string]int{"lo": 1, "eth1": 3})
			managed := 0
			for _, link := range model.Links {
				if link.Managed {
					managed++
				}
			}
			if managed != 2 {
				t.Errorf("managed = %d, want eth1 and dummy0", managed)
			}
			eth, _ := model.Link("eth1")
			if eth.Index != 3 || eth.Type != "ether" || eth.Kind != "veth" {
				t.Errorf("eth1 = %d %q %q", eth.Index, eth.Type, eth.Kind)
			}
			if eth.Operational != "routable" || eth.Addresses[0].Source != "static" {
				t.Errorf("eth1 operational %q source %q", eth.Operational, eth.Addresses[0].Source)
			}
			// Ubuntu keeps an nmcli-made profile in netplan and renders it
			// to /run/NetworkManager/system-connections/netplan-NM-<uuid>.
			file, ok := model.ConfigFor(eth)
			if !ok || file.ID == "" || !strings.HasSuffix(file.Path, ".nmconnection") {
				t.Fatalf("eth1 profile = %+v %v", file, ok)
			}
			if method, _ := file.Get("ipv4", "method"); method != "manual" {
				t.Errorf("ipv4.method = %q", method)
			}
			// Empty settings are dropped, so the profile shows what it says.
			if _, ok := file.Get("ipv6", "dns"); ok {
				t.Error("an empty ipv6.dns should not be listed")
			}
			for _, name := range []string{"lo", "peer1"} {
				link, _ := model.Link(name)
				if link.Managed || link.ReadOnlyReason == "" {
					t.Errorf("%s should be read-only with a reason", name)
				}
			}
			spec := SpecFor(model, eth)
			if spec.Connection != file.ID || spec.DHCP != MethodManual ||
				spec.Address != "192.0.2.10/24" || spec.Gateway != "192.0.2.1" {
				t.Errorf("spec = %+v", spec)
			}
		})
	}
}

func TestSplitTerseEscapes(t *testing.T) {
	got := SplitTerse(`Office\: 2nd floor:uuid:802-11-wireless:wlp2s0:/etc/a\\b`)
	want := []string{"Office: 2nd floor", "uuid", "802-11-wireless", "wlp2s0", `/etc/a\b`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("SplitTerse = %q", got)
	}
}

func TestParseStateAndVersion(t *testing.T) {
	code, state := parseState("100 (connected (externally))")
	if code != 100 || state != "connected (externally)" {
		t.Errorf("parseState = %d %q", code, state)
	}
	if v := ParseVersion("nmcli tool, version 1.56.1-2.fc44\n"); v != "1.56.1-2.fc44" {
		t.Errorf("ParseVersion = %q", v)
	}
}

func TestDHCPLeaseAndOlderSearchKey(t *testing.T) {
	// IP4.DOMAIN is the pre-1.44 name of IP4.SEARCHES; DHCP4.OPTION lines
	// carry "name = value".
	out := "GENERAL.DEVICE:enp1s0\nGENERAL.TYPE:ethernet\nGENERAL.STATE:100 (connected)\n" +
		"IP4.ADDRESS[1]:192.0.2.24/24\nIP4.DOMAIN[1]:example.test\n" +
		"DHCP4.OPTION[1]:dhcp_server_identifier = 192.0.2.1\n" +
		"DHCP4.OPTION[2]:ip_address = 192.0.2.24\nDHCP4.OPTION[3]:dhcp_lease_time = 3600\n"
	devices := ParseDeviceShow(out)
	if len(devices) != 1 || devices[0].Searches[0] != "example.test" {
		t.Fatalf("devices = %+v", devices)
	}
	profile := Profile{UUID: "x", Settings: []network.Setting{{Section: "ipv4", Key: "method", Value: "auto"}}}
	devices[0].ConnectionUUID = "x"
	model := BuildModel(devices, []Profile{profile}, nil)
	link := model.Links[0]
	if !link.DHCP.Enabled || link.DHCP.Server != "192.0.2.1" || link.Addresses[0].Source != "DHCPv4" {
		t.Errorf("link = %+v", link)
	}
}

func FuzzParseDeviceShow(f *testing.F) {
	f.Add("GENERAL.DEVICE:eth0\nGENERAL.STATE:100 (connected)\nIP4.ADDRESS[1]:192.0.2.1/24\n")
	f.Add("GENERAL.DEVICE:x\nDHCP4.OPTION[1]:a = b\n\nGENERAL.DEVICE:y\n")
	f.Fuzz(func(t *testing.T, out string) {
		devices := ParseDeviceShow(out)
		_ = BuildModel(devices, ParseConnectionList(out), nil)
	})
}

func FuzzSplitTerse(f *testing.F) {
	f.Add(`a\:b:c\\d:`)
	f.Fuzz(func(t *testing.T, line string) {
		_ = SplitTerse(line)
	})
}
