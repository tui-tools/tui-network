package networkd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tui-tools/tui-network/internal/network"
)

// read loads a fixture. The fixtures are real output, captured from a machine
// running systemd-networkd, with the hardware addresses and the one routable
// network rewritten into the documentation ranges (192.0.2.0/24,
// 198.51.100.0/24, 02:00:00:00:00:0x).
func read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return string(data)
}

func TestParseListJSON(t *testing.T) {
	links, err := ParseListJSON(read(t, "networkctl-list.json"))
	if err != nil {
		t.Fatalf("ParseListJSON: %v", err)
	}
	if len(links) != 4 {
		t.Fatalf("got %d links, want 4", len(links))
	}

	tests := []struct {
		name        string
		index       int
		linkType    string
		setup       string
		operational string
		mac         string
		mtu         int
		managed     bool
		networkFile string
	}{
		{"lo", 1, "loopback", network.SetupUnmanaged, "carrier", "", 65536, false, ""},
		{"eth0", 2, "ether", network.SetupUnmanaged, "routable",
			"02:00:00:00:00:02", 1500, false, ""},
		{"veth1", 3, "ether", network.SetupUnmanaged, "routable",
			"02:00:00:00:00:03", 1500, false, ""},
		{"veth0", 4, "ether", network.SetupConfigured, "routable",
			"02:00:00:00:00:04", 1500, true,
			"/etc/systemd/network/10-wired.network"},
	}
	for i, want := range tests {
		got := links[i]
		if got.Name != want.name || got.Index != want.index {
			t.Errorf("link %d: got %s/%d, want %s/%d",
				i, got.Name, got.Index, want.name, want.index)
		}
		if got.Type != want.linkType || got.Setup != want.setup ||
			got.Operational != want.operational {
			t.Errorf("%s: got %s/%s/%s, want %s/%s/%s", want.name,
				got.Type, got.Setup, got.Operational,
				want.linkType, want.setup, want.operational)
		}
		if got.MAC != want.mac || got.MTU != want.mtu {
			t.Errorf("%s: got mac %q mtu %d, want %q %d",
				want.name, got.MAC, got.MTU, want.mac, want.mtu)
		}
		if got.Managed != want.managed {
			t.Errorf("%s: managed %v, want %v", want.name, got.Managed, want.managed)
		}
		if got.NetworkFile != want.networkFile {
			t.Errorf("%s: network file %q, want %q",
				want.name, got.NetworkFile, want.networkFile)
		}
		if !got.Managed && got.ReadOnlyReason == "" {
			t.Errorf("%s: an unmanaged link must carry a reason", want.name)
		}
	}
}

func TestParseListJSONAddressesAndGateway(t *testing.T) {
	links, err := ParseListJSON(read(t, "networkctl-list.json"))
	if err != nil {
		t.Fatalf("ParseListJSON: %v", err)
	}
	var veth0 network.Link
	for _, link := range links {
		if link.Name == "veth0" {
			veth0 = link
		}
	}

	if got := veth0.PrimaryAddress(); got != "192.0.2.98/24" {
		t.Errorf("primary address %q, want 192.0.2.98/24", got)
	}
	if got := veth0.Gateway(); got != "192.0.2.1" {
		t.Errorf("gateway %q, want 192.0.2.1", got)
	}
	if len(veth0.Addresses) != 2 {
		t.Fatalf("got %d addresses, want 2", len(veth0.Addresses))
	}
	var dhcp network.Address
	for _, address := range veth0.Addresses {
		if address.Family == "ipv4" {
			dhcp = address
		}
	}
	if dhcp.Source != "DHCPv4" || dhcp.Provider != "192.0.2.1" ||
		dhcp.Scope != "global" || dhcp.Prefix != 24 {
		t.Errorf("dhcp address = %+v", dhcp)
	}
	if !veth0.DHCP.Enabled || veth0.DHCP.Server != "192.0.2.1" ||
		veth0.DHCP.Address != "192.0.2.98" {
		t.Errorf("dhcp lease = %+v", veth0.DHCP)
	}
	if veth0.DHCP.LeaseTimestamp == "" || veth0.DHCP.Timeout1 == "" {
		t.Errorf("the lease clock was not read: %+v", veth0.DHCP)
	}
	if len(veth0.DNS) != 1 || veth0.DNS[0] != "192.0.2.53" {
		t.Errorf("dns = %v, want [192.0.2.53]", veth0.DNS)
	}
}

func TestParseStatusJSON(t *testing.T) {
	link, err := ParseStatusJSON(read(t, "networkctl-status-veth0.json"))
	if err != nil {
		t.Fatalf("ParseStatusJSON: %v", err)
	}
	if link.Name != "veth0" || link.Kind != "veth" || link.Online != "online" {
		t.Errorf("link = %+v", link)
	}
	if !link.Managed {
		t.Errorf("veth0 is configured by networkd, so it must be managed")
	}
	if link.NetworkFile != "/etc/systemd/network/10-wired.network" {
		t.Errorf("network file %q", link.NetworkFile)
	}
}

func TestParseStatusJSONLoopback(t *testing.T) {
	link, err := ParseStatusJSON(read(t, "networkctl-status-lo.json"))
	if err != nil {
		t.Fatalf("ParseStatusJSON: %v", err)
	}
	if link.Name != "lo" || link.MAC != "" {
		t.Errorf("link = %+v, want lo with no hardware address", link)
	}
	if link.Managed {
		t.Errorf("the loopback is unmanaged here, so it must not be writable")
	}
}

func TestParseStatusText(t *testing.T) {
	link := ParseStatusText(read(t, "networkctl-status-veth0.txt"))

	tests := []struct {
		field string
		got   string
		want  string
	}{
		{"name", link.Name, "veth0"},
		{"type", link.Type, "ether"},
		{"kind", link.Kind, "veth"},
		{"setup", link.Setup, network.SetupConfigured},
		{"operational", link.Operational, "routable"},
		{"online", link.Online, "online"},
		{"mac", link.MAC, "02:00:00:00:00:04"},
		{"network file", link.NetworkFile, "/etc/systemd/network/10-wired.network"},
		{"dhcp server", link.DHCP.Server, "192.0.2.1"},
		{"dhcp address", link.DHCP.Address, "192.0.2.98"},
		{"dhcp client id", link.DHCP.ClientID, "IAID:0x945c2505/DUID"},
	}
	for _, test := range tests {
		if test.got != test.want {
			t.Errorf("%s = %q, want %q", test.field, test.got, test.want)
		}
	}
	if link.Index != 4 || link.MTU != 1500 {
		t.Errorf("index %d mtu %d, want 4 and 1500", link.Index, link.MTU)
	}
	if got := link.Gateway(); got != "192.0.2.1" {
		t.Errorf("gateway %q, want 192.0.2.1", got)
	}
	if len(link.DNS) != 1 || link.DNS[0] != "192.0.2.53" {
		t.Errorf("dns = %v", link.DNS)
	}
	// The text form lists the addresses on continuation lines, so both the
	// keyed line and the one under it must land in the model.
	if len(link.Addresses) != 2 {
		t.Errorf("got %d addresses, want 2: %+v", len(link.Addresses), link.Addresses)
	}
}

func TestParseListText(t *testing.T) {
	links := ParseListText(read(t, "networkctl-list.txt"))
	if len(links) != 4 {
		t.Fatalf("got %d links, want 4", len(links))
	}
	if links[0].Name != "lo" || links[0].Type != "loopback" ||
		links[0].Setup != network.SetupUnmanaged {
		t.Errorf("first link = %+v", links[0])
	}
	if links[3].Name != "veth0" || !links[3].Managed {
		t.Errorf("veth0 = %+v, want managed", links[3])
	}
}

func TestParseListTextIgnoresPlaceholders(t *testing.T) {
	// A machine where networkd is not running prints a dash for every state
	// it cannot report. A dash is an absence, not a value.
	links := ParseListText("IDX LINK TYPE     OPERATIONAL SETUP\n" +
		"  1 lo   loopback -           unmanaged\n\n1 links listed.\n")
	if len(links) != 1 {
		t.Fatalf("got %d links, want 1", len(links))
	}
	if links[0].Operational != "" {
		t.Errorf("operational = %q, want empty", links[0].Operational)
	}
}

func TestParseRoutesJSON(t *testing.T) {
	routes, err := ParseRoutesJSON(read(t, "ip-route.json"))
	if err != nil {
		t.Fatalf("ParseRoutesJSON: %v", err)
	}
	if len(routes) != 7 {
		t.Fatalf("got %d routes, want 7", len(routes))
	}
	first := routes[0]
	if first.Destination != "default" || first.Gateway != "198.51.100.1" ||
		first.Link != "eth0" || first.Family != "ipv4" {
		t.Errorf("first route = %+v", first)
	}
	second := routes[1]
	if second.Protocol != "dhcp" || second.Metric != 1024 ||
		second.Source != "192.0.2.98" {
		t.Errorf("second route = %+v", second)
	}
}

func TestParseResolvectlDNS(t *testing.T) {
	perLink, global := ParseResolvectlDNS(read(t, "resolvectl-dns.txt"))
	if len(global) != 0 {
		t.Errorf("global dns = %v, want none", global)
	}
	want := []string{"192.0.2.53", "192.0.2.53", "2001:db8::53", "2001:db8::53"}
	if got := perLink["enp44s0"]; len(got) != len(want) {
		t.Errorf("enp44s0 dns = %v, want %v", got, want)
	}
	// wlo1's list wraps onto a continuation line, which is the case that
	// makes this parser worth having.
	if got := perLink["wlo1"]; len(got) != 4 {
		t.Errorf("wlo1 dns = %v, want four servers including the wrapped one", got)
	}
	if _, ok := perLink["tailscale0"]; !ok {
		t.Errorf("a link with no servers must still be recorded")
	}
}

func TestParseResolvectlDomain(t *testing.T) {
	perLink, global := ParseResolvectlDNS(read(t, "resolvectl-domain.txt"))
	if len(global) != 0 {
		t.Errorf("global domains = %v, want none", global)
	}
	if domains := perLink["enp44s0"]; len(domains) != 0 {
		t.Errorf("enp44s0 domains = %v, want none", domains)
	}
}

func TestParseNetworkFile(t *testing.T) {
	file := ParseNetworkFile(demoConfigPath, demoConfig)
	if file.MatchName != "enp1s0" {
		t.Errorf("match name = %q, want enp1s0", file.MatchName)
	}
	if dhcp, ok := file.Get("Network", "DHCP"); !ok || dhcp != "ipv4" {
		t.Errorf("DHCP = %q (%v), want ipv4", dhcp, ok)
	}
	if servers := file.All("Network", "DNS"); len(servers) != 1 ||
		servers[0] != "192.0.2.53" {
		t.Errorf("DNS = %v", servers)
	}
	// The comment header and the blank lines are kept in Raw and dropped from
	// the settings.
	if len(file.Settings) != 4 {
		t.Errorf("got %d settings, want 4: %+v", len(file.Settings), file.Settings)
	}
	if file.Raw != demoConfig {
		t.Errorf("the raw text must be preserved verbatim")
	}
}

func TestParseNetworkFileIgnoresComments(t *testing.T) {
	file := ParseNetworkFile("/etc/systemd/network/x.network",
		"# a comment\n; another\n[Match]\nName=eth0\nnot a setting\n")
	if len(file.Settings) != 1 || file.Settings[0].Key != "Name" {
		t.Errorf("settings = %+v", file.Settings)
	}
}

// TestParseListJSONRealGuests pins the two shapes `networkctl --json=short
// list` really has on the machines tui-lab boots.
//
// They are not the same shape. systemd 261 adds a top-level "Routes" array,
// the *String rendering of every address ("AddressString",
// "DestinationString"), and a whole decoded DHCP "Message" inside the lease;
// systemd 255 has none of those and spells one field both
// "PreferredLifetimeUSec" and "PreferredLifetimeUsec". The parser reads only
// the fields it names, which is what makes both work — this test is what keeps
// that true, because a fixture reconstructed by hand would have neither shape.
//
// Captured from the lab guests and rewritten into the documentation ranges:
// QEMU's 10.0.2.0/24, fec0::/64 and 52:54:00 MAC never reach the repository.
func TestParseListJSONRealGuests(t *testing.T) {
	tests := []struct {
		name        string
		fixture     string
		link        string
		networkFile string
		gateway     string
	}{
		{"systemd 255, Ubuntu 24.04 with netplan",
			"networkctl-list-systemd255.json", "enp0s4",
			"/run/systemd/network/10-netplan-enp0s4.network", "192.0.2.1"},
		{"systemd 261, Omarchy Server 4.0.1",
			"networkctl-list-systemd261.json", "enp0s4",
			"/etc/systemd/network/20-wired.network", "192.0.2.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			links, err := ParseListJSON(read(t, tt.fixture))
			if err != nil {
				t.Fatalf("ParseListJSON: %v", err)
			}
			if len(links) != 2 {
				t.Fatalf("got %d links, want lo and %s", len(links), tt.link)
			}
			if links[0].Name != "lo" || links[0].Managed {
				t.Errorf("link 0 = %+v, want an unmanaged lo", links[0])
			}

			link := links[1]
			if link.Name != tt.link {
				t.Fatalf("link 1 = %q, want %q", link.Name, tt.link)
			}
			if !link.Managed || link.Setup != network.SetupConfigured {
				t.Errorf("%s setup = %q, managed = %v; want configured and managed",
					link.Name, link.Setup, link.Managed)
			}
			if link.NetworkFile != tt.networkFile {
				t.Errorf("network file = %q, want %q",
					link.NetworkFile, tt.networkFile)
			}
			// The MAC and the addresses come back as bytes, so a parser that
			// dropped the rebuild would leave these empty.
			if link.MAC != "02:00:00:00:00:02" {
				t.Errorf("MAC = %q, want the scrubbed 02:00:00:00:00:02", link.MAC)
			}
			if !hasAddress(link, "192.0.2.15") {
				t.Errorf("addresses = %+v, want the DHCP lease 192.0.2.15",
					link.Addresses)
			}
			if len(link.Gateways) == 0 || link.Gateways[0] != tt.gateway {
				t.Errorf("gateways = %v, want %s first", link.Gateways, tt.gateway)
			}
			// Both guests lease their address, and the lease clock is the one
			// field that only exists on the JSON path.
			if !link.DHCP.Enabled || link.DHCP.LeaseTimestamp == "" {
				t.Errorf("DHCP = %+v, want an enabled client with a lease",
					link.DHCP)
			}
		})
	}
}

// TestParseStatusJSONRealGuests reads the same two guests through
// `networkctl status --json=short`, which is the detail view's own path and a
// different envelope: one interface object rather than a list.
func TestParseStatusJSONRealGuests(t *testing.T) {
	for _, fixture := range []string{
		"networkctl-status-systemd255.json",
		"networkctl-status-systemd261.json",
	} {
		t.Run(fixture, func(t *testing.T) {
			link, err := ParseStatusJSON(read(t, fixture))
			if err != nil {
				t.Fatalf("ParseStatusJSON: %v", err)
			}
			if link.Name != "enp0s4" || !link.Managed {
				t.Fatalf("link = %+v, want a managed enp0s4", link)
			}
			if link.MTU != 1500 || link.Type != "ether" {
				t.Errorf("type/MTU = %q/%d, want ether/1500", link.Type, link.MTU)
			}
			if !hasAddress(link, "192.0.2.15") {
				t.Errorf("addresses = %+v, want 192.0.2.15", link.Addresses)
			}
		})
	}
}

// hasAddress reports whether a link carries an address, by its text form.
func hasAddress(link network.Link, address string) bool {
	for _, a := range link.Addresses {
		if a.Address == address {
			return true
		}
	}
	return false
}
