package dhcpd

import (
	"strings"
	"testing"

	"github.com/tui-tools/tui-network/internal/dhcp"
)

// routerUnit is the LAN unit omarchy-router-nics writes: the gateway address
// and a DHCP server that hands this router out as the clients' resolver.
const routerUnit = `# Omarchy Router: the internal LAN, the gateway address and its DHCP server.
[Match]
Name=enp2s0

[Network]
Address=10.55.0.1/24
DHCPServer=yes

[DHCPServer]
EmitDNS=yes
DNS=_server_address
`

// TestParseNetworkdUnitReadsTheRouterUnit covers the read path against the
// exact file the router profile writes.
func TestParseNetworkdUnitReadsTheRouterUnit(t *testing.T) {
	unit := ParseNetworkdUnit([]NetworkdFile{
		{Path: "/etc/systemd/network/20-omarchy-lan.network", Raw: routerUnit},
	})

	if unit.Link != "enp2s0" {
		t.Errorf("link is %q, want enp2s0", unit.Link)
	}
	if unit.Address != "10.55.0.1/24" {
		t.Errorf("address is %q, want 10.55.0.1/24", unit.Address)
	}
	if !unit.Enabled || !unit.HasSection {
		t.Errorf("the unit is not read as running a DHCP server: %+v", unit)
	}
	if len(unit.DNS) != 1 || unit.DNS[0] != "_server_address" {
		t.Errorf("advertised DNS is %v, want [_server_address]", unit.DNS)
	}
	if unit.PoolOffset != 0 || unit.PoolSize != 0 {
		t.Errorf("the unit sets no pool keys, got offset %d size %d",
			unit.PoolOffset, unit.PoolSize)
	}
}

// TestParseNetworkdUnitFoldsDropins covers systemd's merge rules, which are the
// reason the drop-in can own a field at all: a scalar set again wins, a list
// cleared by an empty assignment starts over, and a static lease section is
// additive.
func TestParseNetworkdUnitFoldsDropins(t *testing.T) {
	dropin := `[DHCPServer]
PoolOffset=100
PoolSize=50
DefaultLeaseTimeSec=30min
DNS=
EmitDNS=yes
DNS=10.55.0.1 9.9.9.9
SendOption=
SendOption=15:string:lan.example.test

[DHCPServerStaticLease]
MACAddress=00:00:5E:00:53:01
Address=10.55.0.10
`
	unit := ParseNetworkdUnit([]NetworkdFile{
		{Path: "/etc/systemd/network/20-lan.network", Raw: routerUnit},
		{Path: "/etc/systemd/network/20-lan.network.d/50-tui-network-dhcp.conf",
			Raw: dropin},
	})

	if unit.PoolOffset != 100 || unit.PoolSize != 50 {
		t.Errorf("pool is offset %d size %d, want 100/50", unit.PoolOffset, unit.PoolSize)
	}
	if unit.DefaultLeaseTimeSec != "30min" {
		t.Errorf("lease time is %q, want 30min", unit.DefaultLeaseTimeSec)
	}
	if got := strings.Join(unit.DNS, " "); got != "10.55.0.1 9.9.9.9" {
		t.Errorf("advertised DNS is %q; the drop-in's empty DNS= must clear "+
			"the unit's _server_address", got)
	}
	if unit.Domain != "lan.example.test" {
		t.Errorf("domain is %q, want lan.example.test", unit.Domain)
	}
	if len(unit.Leases) != 1 {
		t.Fatalf("read %d static leases, want 1: %+v", len(unit.Leases), unit.Leases)
	}
	lease := unit.Leases[0]
	if lease.MAC != "00:00:5e:00:53:01" || lease.IP != "10.55.0.10" {
		t.Errorf("the static lease is %+v", lease)
	}
	if !strings.HasSuffix(lease.Source, NetworkdDropinName) {
		t.Errorf("the lease's source is %q, want the drop-in it was read from",
			lease.Source)
	}
	if len(unit.Dropins) != 1 {
		t.Errorf("the unit records %d drop-ins, want 1", len(unit.Dropins))
	}
}

// TestPoolRangeAppliesSystemdDefaults pins the arithmetic to systemd's own: a
// zero offset means one, a zero size means the rest of the subnet up to but not
// including the broadcast address, and the server's own address may fall inside
// the pool.
func TestPoolRangeAppliesSystemdDefaults(t *testing.T) {
	cases := []struct {
		name           string
		address        string
		offset, size   int
		wantStart, end string
	}{
		{"defaults", "10.55.0.1/24", 0, 0, "10.55.0.1", "10.55.0.254"},
		{"offset only", "10.55.0.1/24", 100, 0, "10.55.0.100", "10.55.0.254"},
		{"offset and size", "10.55.0.1/24", 100, 50, "10.55.0.100", "10.55.0.149"},
		{"a /22", "10.55.0.1/22", 1, 0, "10.55.0.1", "10.55.3.254"},
		{"a /30", "192.0.2.1/30", 0, 0, "192.0.2.1", "192.0.2.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end, err := PoolRange(tc.address, tc.offset, tc.size)
			if err != nil {
				t.Fatalf("PoolRange: %v", err)
			}
			if start != tc.wantStart || end != tc.end {
				t.Errorf("pool is %s–%s, want %s–%s", start, end, tc.wantStart, tc.end)
			}
		})
	}
}

// TestPoolRangeRefusesWhatSystemdWould covers the ranges the server itself
// would not configure.
func TestPoolRangeRefusesWhatSystemdWould(t *testing.T) {
	cases := []struct {
		name         string
		address      string
		offset, size int
	}{
		{"no prefix", "10.55.0.1", 0, 0},
		{"not an address", "lan", 0, 0},
		{"a /31 has nothing to hand out", "10.55.0.1/31", 0, 0},
		{"a size past the broadcast address", "10.55.0.1/24", 200, 100},
		{"an offset past the subnet", "10.55.0.1/24", 255, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := PoolRange(tc.address, tc.offset, tc.size); err == nil {
				t.Errorf("PoolRange(%q, %d, %d) was accepted",
					tc.address, tc.offset, tc.size)
			}
		})
	}
}

// TestPoolOffsetSizeIsPoolRangeBackwards covers the key the p prompt writes,
// and the ranges it refuses.
func TestPoolOffsetSizeIsPoolRangeBackwards(t *testing.T) {
	offset, size, err := PoolOffsetSize("10.55.0.1/24", "10.55.0.100", "10.55.0.199")
	if err != nil {
		t.Fatalf("PoolOffsetSize: %v", err)
	}
	if offset != 100 || size != 100 {
		t.Fatalf("offset %d size %d, want 100/100", offset, size)
	}
	start, end, err := PoolRange("10.55.0.1/24", offset, size)
	if err != nil || start != "10.55.0.100" || end != "10.55.0.199" {
		t.Errorf("the round trip gives %s–%s (%v)", start, end, err)
	}

	refused := []struct{ name, start, end string }{
		{"backwards", "10.55.0.200", "10.55.0.100"},
		{"the subnet address", "10.55.0.0", "10.55.0.100"},
		{"the broadcast address", "10.55.0.100", "10.55.0.255"},
		{"outside the subnet", "10.55.1.10", "10.55.1.20"},
		{"not an address", "lan", "10.55.0.20"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := PoolOffsetSize("10.55.0.1/24", tc.start, tc.end); err == nil {
				t.Errorf("%s–%s was accepted", tc.start, tc.end)
			}
		})
	}
}

// TestRenderNetworkdDropinClearsBeforeItSets covers the rule that makes a
// drop-in able to own an advertised list at all: without the clear, the unit's
// own DNS= would still be in the lease.
func TestRenderNetworkdDropinClearsBeforeItSets(t *testing.T) {
	text, err := RenderNetworkdDropin(NetworkdDropin{
		Link:       "enp2s0",
		PoolOffset: 100,
		PoolSize:   100,
		Options: dhcp.Options{
			DNS:       []string{"10.55.0.1", "_server_address"},
			NTP:       []string{"10.55.0.1"},
			Gateway:   "10.55.0.1",
			Domain:    "lan.example.test",
			LeaseTime: "1h",
		},
		Leases: []dhcp.Reservation{{MAC: "00:00:5E:00:53:01", IP: "10.55.0.10"}},
	})
	if err != nil {
		t.Fatalf("RenderNetworkdDropin: %v", err)
	}
	for _, want := range []string{
		"[DHCPServer]\n",
		"PoolOffset=100\n",
		"PoolSize=100\n",
		"DefaultLeaseTimeSec=1h\n",
		"DNS=\nEmitDNS=yes\nDNS=10.55.0.1 _server_address\n",
		"NTP=\nEmitNTP=yes\nNTP=10.55.0.1\n",
		"EmitRouter=yes\nRouter=10.55.0.1\n",
		"SendOption=\nSendOption=15:string:lan.example.test\n",
		"[DHCPServerStaticLease]\nMACAddress=00:00:5e:00:53:01\nAddress=10.55.0.10\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the drop-in does not contain %q:\n%s", want, text)
		}
	}

	// What it renders is what it reads back.
	unit := ParseNetworkdUnit([]NetworkdFile{{Path: "d.conf", Raw: text}})
	if unit.PoolOffset != 100 || unit.PoolSize != 100 ||
		unit.Domain != "lan.example.test" || len(unit.Leases) != 1 {
		t.Errorf("the rendered drop-in does not read back: %+v", unit)
	}
}

// TestRenderNetworkdDropinRefusesUnitFileInjection is the safety property: every
// value goes into a systemd unit file, so a newline, a section header or a
// value that is not what it claims to be must not survive the renderer.
func TestRenderNetworkdDropinRefusesUnitFileInjection(t *testing.T) {
	cases := []struct {
		name string
		spec NetworkdDropin
	}{
		{"a DNS server that is not an address",
			NetworkdDropin{Options: dhcp.Options{DNS: []string{"10.55.0.1\nRouter=6.6.6.6"}}}},
		{"an NTP server that is not an address",
			NetworkdDropin{Options: dhcp.Options{NTP: []string{"[DHCPServer]"}}}},
		{"a gateway that is not an address",
			NetworkdDropin{Options: dhcp.Options{Gateway: "the router"}}},
		{"a domain that is not a domain",
			NetworkdDropin{Options: dhcp.Options{Domain: "lan\nSendOption=1:string:x"}}},
		{"a lease time that is not a time span",
			NetworkdDropin{Options: dhcp.Options{LeaseTime: "1 hour"}}},
		{"a static lease with no address",
			NetworkdDropin{Leases: []dhcp.Reservation{{MAC: "00:00:5e:00:53:01"}}}},
		{"a static lease with a wildcard MAC",
			NetworkdDropin{Leases: []dhcp.Reservation{
				{MAC: "00:00:5e:00:53:*", IP: "10.55.0.10"}}}},
		{"a negative pool", NetworkdDropin{PoolOffset: -1, PoolSize: 10}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RenderNetworkdDropin(tc.spec); err == nil {
				t.Errorf("%s was accepted", tc.name)
			}
		})
	}
}

// TestNetworkdDropinPathIsRefusedOutsideItsPlace covers the write boundary: the
// only file this backend may install is its own drop-in, in a .network.d
// directory under /etc/systemd/network.
func TestNetworkdDropinPathIsRefusedOutsideItsPlace(t *testing.T) {
	want := "/etc/systemd/network/20-omarchy-lan.network.d/" + NetworkdDropinName
	if got := NetworkdDropinPath("/usr/lib/systemd/network/20-omarchy-lan.network"); got != want {
		t.Errorf("drop-in path is %q, want %q", got, want)
	}
	if err := checkNetworkdPath(want); err != nil {
		t.Errorf("its own drop-in is refused: %v", err)
	}
	for _, path := range []string{
		"/etc/passwd",
		"/etc/systemd/network/20-lan.network",
		"/etc/systemd/network/20-lan.network.d/99-other.conf",
		"/etc/systemd/network/20-lan.netdev.d/" + NetworkdDropinName,
	} {
		if err := checkNetworkdPath(path); err == nil {
			t.Errorf("%s was accepted as a write target", path)
		}
	}
}

// TestParseNetworkctlLeasesReadsTheOfferedList covers the lease read path: the
// list networkctl renders from the DHCPServer bus property, which is where the
// leases a networkd server has offered are published.
func TestParseNetworkctlLeasesReadsTheOfferedList(t *testing.T) {
	out := `● 3: enp2s0
                  Network File: /etc/systemd/network/20-omarchy-lan.network
                         State: routable (configured)
           Offered DHCP leases: 10.55.0.100 (to 00:00:5e:00:53:0a)
                                10.55.0.101 (to SOMEHOST)
                    Statistics: whatever
`
	leases := ParseNetworkctlLeases(out)
	if len(leases) != 2 {
		t.Fatalf("read %d leases, want 2: %+v", len(leases), leases)
	}
	if leases[0].IP != "10.55.0.100" || leases[0].MAC != "00:00:5e:00:53:0a" {
		t.Errorf("the first lease is %+v", leases[0])
	}
	if leases[1].ClientID != "SOMEHOST" || leases[1].MAC != "" {
		t.Errorf("a client id that is not a MAC must stay a client id: %+v", leases[1])
	}
	if got := ParseNetworkctlLeases("           Offered DHCP leases: none\n"); len(got) != 0 {
		t.Errorf("an empty list read as %+v", got)
	}
}

// TestNetworkdModelDerivesTheScreen covers what the DHCP screen is handed: one
// pool per unit, the static leases as reservations, and an options view seeded
// from what is in effect.
func TestNetworkdModelDerivesTheScreen(t *testing.T) {
	unit := ParseNetworkdUnit([]NetworkdFile{
		{Path: "/etc/systemd/network/20-lan.network", Raw: routerUnit},
	})
	model := NetworkdModel([]NetworkdUnit{unit})

	if len(model.Pools) != 1 {
		t.Fatalf("derived %d pools, want 1", len(model.Pools))
	}
	pool := model.Pools[0]
	if pool.Start != "10.55.0.1" || pool.End != "10.55.0.254" ||
		pool.Netmask != "255.255.255.0" || pool.Name != "enp2s0" {
		t.Errorf("the pool is %+v", pool)
	}
	if len(model.Options.DNS) != 1 || model.Options.DNS[0] != "_server_address" {
		t.Errorf("advertised DNS is %v", model.Options.DNS)
	}
	// The editor opens on what is in effect, because the drop-in it writes
	// replaces the lists rather than adding to them.
	if len(model.OwnOptions.DNS) != 1 {
		t.Errorf("the options editor is not seeded from the effective options: %+v",
			model.OwnOptions)
	}
}

// TestNetworkdOptionsHonoursTheEmitSwitches covers EmitDNS=no and friends: a
// switch turned off means the client is handed nothing, whatever the list says.
func TestNetworkdOptionsHonoursTheEmitSwitches(t *testing.T) {
	unit := ParseNetworkdUnit([]NetworkdFile{{Path: "u", Raw: `[Network]
DHCPServer=yes
Address=10.55.0.1/24

[DHCPServer]
EmitDNS=no
DNS=10.55.0.1
EmitRouter=no
Router=10.55.0.254
`}})
	opts := NetworkdOptions(unit)
	if len(opts.DNS) != 0 {
		t.Errorf("EmitDNS=no still advertises %v", opts.DNS)
	}
	if opts.Gateway != "" {
		t.Errorf("EmitRouter=no still advertises %q", opts.Gateway)
	}
}

// TestNewNetworkdLeaseRefusesAClash covers the check that keeps two static
// leases from fighting over one client or one address.
func TestNewNetworkdLeaseRefusesAClash(t *testing.T) {
	existing := []dhcp.Reservation{{MAC: "00:00:5e:00:53:01", IP: "10.55.0.10"}}
	if _, err := NewNetworkdLease(existing,
		dhcp.Reservation{MAC: "00:00:5E:00:53:01", IP: "10.55.0.11"}); err == nil {
		t.Error("a second lease for the same MAC was accepted")
	}
	if _, err := NewNetworkdLease(existing,
		dhcp.Reservation{MAC: "00:00:5e:00:53:02", IP: "10.55.0.10"}); err == nil {
		t.Error("a second lease for the same address was accepted")
	}
	lease, err := NewNetworkdLease(existing,
		dhcp.Reservation{MAC: "00:00:5E:00:53:02", IP: "10.55.0.11", Hostname: "nas"})
	if err != nil {
		t.Fatalf("a free MAC and address were refused: %v", err)
	}
	if lease.MAC != "00:00:5e:00:53:02" || lease.Hostname != "" {
		t.Errorf("the stored lease is %+v; networkd's static lease has no "+
			"hostname key and the MAC is normalised", lease)
	}
}

// TestRemoveNetworkdLease covers the removal by either key, and the refusal of
// one the drop-in does not declare.
func TestRemoveNetworkdLease(t *testing.T) {
	existing := []dhcp.Reservation{
		{MAC: "00:00:5e:00:53:01", IP: "10.55.0.10"},
		{MAC: "00:00:5e:00:53:02", IP: "10.55.0.11"},
	}
	left, err := RemoveNetworkdLease(existing, dhcp.Reservation{IP: "10.55.0.10"})
	if err != nil || len(left) != 1 || left[0].MAC != "00:00:5e:00:53:02" {
		t.Fatalf("removing by address left %+v (%v)", left, err)
	}
	if _, err := RemoveNetworkdLease(existing,
		dhcp.Reservation{MAC: "00:00:5e:00:53:09"}); err == nil {
		t.Error("removing a lease that is not there was accepted")
	}
}

// TestFakeNetworkdAppliesItsOwnWrites covers the demo backend end to end: the
// plan it builds installs its own drop-in, and re-reading shows the change.
func TestFakeNetworkdAppliesItsOwnWrites(t *testing.T) {
	f := NewFakeNetworkd()
	model, _ := f.Load(t.Context())
	if len(model.Pools) != 1 || model.Pools[0].Start != "10.55.0.100" {
		t.Fatalf("the sample router's pool is %+v", model.Pools)
	}

	plan, err := f.BuildSetPoolRange(model.Pools[0], "10.55.0.50", "10.55.0.99")
	if err != nil {
		t.Fatalf("BuildSetPoolRange: %v", err)
	}
	if len(plan.Commands) != 2 ||
		plan.Commands[0].Argv[0] != "install" ||
		strings.Join(plan.Commands[1].Argv, " ") != "networkctl reload" {
		t.Fatalf("the plan is not install + reload: %+v", plan.Commands)
	}
	if !strings.Contains(plan.Diff, "+PoolOffset=50") ||
		!strings.Contains(plan.Diff, "+PoolSize=50") {
		t.Errorf("the diff does not show the new pool:\n%s", plan.Diff)
	}
	for _, cmd := range plan.Commands {
		if _, err := f.Run(t.Context(), cmd); err != nil {
			t.Fatalf("running %v: %v", cmd.Argv, err)
		}
	}
	model, _ = f.Load(t.Context())
	if model.Pools[0].Start != "10.55.0.50" || model.Pools[0].End != "10.55.0.99" {
		t.Errorf("after applying, the pool is %+v", model.Pools[0])
	}
}

// TestFakeNetworkdRefusesRemovingAUnitLease covers the honest refusal: a
// [DHCPServerStaticLease] the unit itself declares cannot be taken back by a
// drop-in, so the tool says so instead of writing a file that does nothing.
func TestFakeNetworkdRefusesRemovingAUnitLease(t *testing.T) {
	f := NewFakeNetworkd()
	_, err := f.BuildRemoveReservation(dhcp.Reservation{
		MAC: "00:00:5e:00:53:aa", IP: "10.55.0.9",
		Source: demoNetworkdUnitPath,
	})
	if err == nil || !strings.Contains(err.Error(), "does not rewrite") {
		t.Errorf("removing a unit-declared lease gave %v", err)
	}
}

// TestHasSubnetKeepsSystemdsOwnTemplatesOut covers the filter that keeps the
// container and VM units systemd itself ships off the router's DHCP screen:
// they run a DHCP server on a null address, so there is no pool to show.
func TestHasSubnetKeepsSystemdsOwnTemplatesOut(t *testing.T) {
	template := ParseNetworkdUnit([]NetworkdFile{{
		Path: "/usr/lib/systemd/network/80-container-ve.network",
		Raw: `[Match]
Name=ve-*

[Network]
Address=0.0.0.0/28
DHCPServer=yes
`}})
	if !template.Enabled {
		t.Fatalf("the template unit does declare a DHCP server")
	}
	if template.HasSubnet() {
		t.Errorf("a null address must not read as a subnet: %q", template.Address)
	}
	if _, ok := NetworkdPool(template); ok {
		t.Errorf("a null address must not produce a pool")
	}

	lan := ParseNetworkdUnit([]NetworkdFile{
		{Path: "/etc/systemd/network/20-lan.network", Raw: routerUnit}})
	if !lan.HasSubnet() {
		t.Errorf("the router's LAN unit has a subnet, got %q", lan.Address)
	}
}
