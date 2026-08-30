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
