package dhcpd

import (
	"strings"
	"testing"
	"time"

	"github.com/tui-tools/tui-network/internal/dhcp"
)

// refNow is a fixed clock the lease tests measure expiries against, so a test
// never depends on the wall clock.
var refNow = time.Unix(1893450000, 0)

func TestParseDnsmasqLeases(t *testing.T) {
	raw := mustReadFixture(t, "dnsmasq.leases")
	leases := ParseDnsmasqLeases(raw, refNow)
	if len(leases) != 4 {
		t.Fatalf("got %d leases, want 4", len(leases))
	}

	first := leases[0]
	if first.MAC != "00:00:5e:00:53:01" || first.IP != "192.0.2.50" ||
		first.Hostname != "laptop" || first.Family != "ipv4" {
		t.Fatalf("first lease parsed wrong: %+v", first)
	}
	if !strings.HasPrefix(first.Expiry, "in ") {
		t.Errorf("a future lease should read as remaining time, got %q", first.Expiry)
	}
	// The "*" placeholders mean no hostname and no client id.
	if leases[2].Hostname != "" {
		t.Errorf("a * hostname should be empty, got %q", leases[2].Hostname)
	}
	if leases[2].Expiry != "never" {
		t.Errorf("a zero expiry should read never, got %q", leases[2].Expiry)
	}
	// The v6 lease: the IAID in field two is not a MAC, so it is not shown as one.
	v6 := leases[3]
	if v6.Family != "ipv6" || v6.IP != "2001:db8::5" {
		t.Errorf("v6 lease parsed wrong: %+v", v6)
	}
	if v6.MAC != "" {
		t.Errorf("a v6 lease's IAID must not be presented as a MAC, got %q", v6.MAC)
	}
}

func TestParseDnsmasqConf(t *testing.T) {
	raw := mustReadFixture(t, "dnsmasq.conf")
	pools, reservations := ParseDnsmasqConf("/etc/dnsmasq.conf", raw)

	if len(pools) != 4 {
		t.Fatalf("got %d pools, want 4", len(pools))
	}
	if pools[0].Start != "192.0.2.50" || pools[0].End != "192.0.2.150" ||
		pools[0].Netmask != "255.255.255.0" || pools[0].LeaseTime != "12h" {
		t.Errorf("plain pool parsed wrong: %+v", pools[0])
	}
	if pools[1].Name != "tag:guest" || pools[1].LeaseTime != "1h" {
		t.Errorf("tagged pool parsed wrong: %+v", pools[1])
	}
	if pools[2].Family != "ipv6" || pools[2].PrefixLen != 64 || pools[2].LeaseTime != "3h" {
		t.Errorf("v6 pool parsed wrong: %+v", pools[2])
	}

	if len(reservations) != 4 {
		t.Fatalf("got %d reservations, want 4", len(reservations))
	}
	if reservations[0].MAC != "00:00:5e:00:53:01" || reservations[0].IP != "192.0.2.10" ||
		reservations[0].Hostname != "printer" {
		t.Errorf("reservation parsed wrong: %+v", reservations[0])
	}
	// The id: form is read as a client id, not a MAC.
	if reservations[2].ClientID != "00:01:00:01:2b:3c:4d:5e" {
		t.Errorf("id: reservation parsed wrong: %+v", reservations[2])
	}
	// The bracketed v6 address is unwrapped.
	if reservations[3].IP != "2001:db8::20" || reservations[3].Family != "ipv6" {
		t.Errorf("v6 reservation parsed wrong: %+v", reservations[3])
	}
	// Every reservation remembers the file it came from, so a removal edits it.
	for _, r := range reservations {
		if r.Source != "/etc/dnsmasq.conf" {
			t.Errorf("reservation lost its source file: %+v", r)
		}
	}
}

func TestRenderReservationLine(t *testing.T) {
	line, err := RenderReservationLine(dhcp.Reservation{
		MAC: "00:00:5e:00:53:10", IP: "192.0.2.20", Hostname: "nas"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if line != "dhcp-host=00:00:5e:00:53:10,192.0.2.20,nas" {
		t.Errorf("line = %q", line)
	}
	// A bad MAC is refused rather than written.
	if _, err := RenderReservationLine(dhcp.Reservation{MAC: "not-a-mac", IP: "192.0.2.20"}); err == nil {
		t.Error("a non-MAC reservation should be refused")
	}
}

func TestAddReservationText(t *testing.T) {
	// Into an empty (not yet created) file, the header is written first.
	got, err := AddReservationText("", dhcp.Reservation{
		MAC: "00:00:5e:00:53:10", IP: "192.0.2.20", Hostname: "nas"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(got, "Written by tui-network") {
		t.Errorf("a new managed file should carry the tool header:\n%s", got)
	}
	if !strings.HasSuffix(got, "dhcp-host=00:00:5e:00:53:10,192.0.2.20,nas\n") {
		t.Errorf("the reservation line should be appended:\n%s", got)
	}

	// Adding the same reservation twice is refused rather than duplicated.
	if _, err := AddReservationText(got, dhcp.Reservation{
		MAC: "00:00:5e:00:53:10", IP: "192.0.2.20", Hostname: "nas"}); err == nil {
		t.Error("a duplicate reservation should be refused")
	}
}

func TestRemoveReservationText(t *testing.T) {
	before := "dhcp-host=00:00:5e:00:53:01,192.0.2.10,printer\n" +
		"dhcp-host=00:00:5e:00:53:02,192.0.2.11,nas\n"
	got, err := RemoveReservationText(before, dhcp.Reservation{MAC: "00:00:5e:00:53:01"})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if strings.Contains(got, "00:00:5e:00:53:01") {
		t.Errorf("the reservation should be gone:\n%s", got)
	}
	if !strings.Contains(got, "00:00:5e:00:53:02") {
		t.Errorf("the other reservation should survive:\n%s", got)
	}
	// Removing by address works too.
	if _, err := RemoveReservationText(before, dhcp.Reservation{IP: "192.0.2.11"}); err != nil {
		t.Errorf("remove by address: %v", err)
	}
	// A reservation that is not there is an error, not a silent no-op.
	if _, err := RemoveReservationText(before, dhcp.Reservation{MAC: "00:00:5e:00:53:ff"}); err == nil {
		t.Error("removing an absent reservation should error")
	}
}

func TestSetPoolRangeText(t *testing.T) {
	before := "dhcp-range=192.0.2.50,192.0.2.150,255.255.255.0,12h\n"
	orig := dhcp.Pool{Start: "192.0.2.50", End: "192.0.2.150", Source: "/etc/dnsmasq.conf"}
	got, err := SetPoolRangeText(before, orig, "192.0.2.40", "192.0.2.200")
	if err != nil {
		t.Fatalf("set range: %v", err)
	}
	want := "dhcp-range=192.0.2.40,192.0.2.200,255.255.255.0,12h\n"
	if got != want {
		t.Errorf("range not adjusted in place:\ngot  %q\nwant %q", got, want)
	}

	// The two new addresses must be a matching pair of addresses.
	if _, err := SetPoolRangeText(before, orig, "nope", "192.0.2.200"); err == nil {
		t.Error("a non-address start should be refused")
	}
	if _, err := SetPoolRangeText(before, orig, "192.0.2.40", "2001:db8::1"); err == nil {
		t.Error("a mixed-family range should be refused")
	}
	// A range that is not present cannot be adjusted.
	missing := dhcp.Pool{Start: "10.0.0.1", End: "10.0.0.9", Source: "/etc/dnsmasq.conf"}
	if _, err := SetPoolRangeText(before, missing, "192.0.2.40", "192.0.2.200"); err == nil {
		t.Error("adjusting an absent range should error")
	}
}
