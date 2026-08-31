package dhcpd

import (
	"strings"
	"testing"

	"github.com/tui-tools/tui-network/internal/dhcp"
)

// The parsers in this package turn files tui-network did not write — dnsmasq
// lease lines and configuration, Kea JSON configuration and lease CSV — into
// the model the DHCP screen shows and the reservations the tool then offers to
// edit. A parser that invents an address, or a pool with no range, is how a
// tool ends up previewing an edit against a line it never really read. The
// seeds are the fixtures plus the shapes a real capture never has.
//
// `go test` runs the seeds on every commit; `go test -fuzz=FuzzParseX
// ./internal/dhcpd/` explores past them locally.

// seedFuzz adds a fixture and the usual degenerate inputs to a corpus.
func seedFuzz(f *testing.F, name string) {
	f.Helper()
	if name != "" {
		f.Add(mustReadFixtureF(f, name))
	}
	f.Add("")
	f.Add("\n\n\n")
	f.Add(" , , ")
	f.Add("=")
	f.Add("{}")
	f.Add("[]")
}

// checkLease asserts what every caller of a lease may assume: an address it can
// print, and a family it can branch on.
func checkLease(t *testing.T, l dhcp.Lease) {
	t.Helper()
	if l.IP == "" || strings.ContainsAny(l.IP, " \t\n") {
		t.Fatalf("lease has no clean address: %+v", l)
	}
	checkFamily(t, l.Family, l.IP)
}

// checkPool asserts a pool always has a first address and a coherent family.
func checkPool(t *testing.T, p dhcp.Pool) {
	t.Helper()
	if p.Start == "" || strings.ContainsAny(p.Start, " \t\n") {
		t.Fatalf("pool has no clean start address: %+v", p)
	}
	checkFamily(t, p.Family, p.Start)
}

// checkReservation asserts a reservation always identifies a client somehow.
func checkReservationParsed(t *testing.T, r dhcp.Reservation) {
	t.Helper()
	if r.MAC == "" && r.ClientID == "" && r.IP == "" {
		t.Fatalf("reservation identifies nothing: %+v", r)
	}
	if r.Family != "" && r.Family != "ipv4" && r.Family != "ipv6" {
		t.Fatalf("reservation family is not a family: %+v", r)
	}
}

// checkFamily asserts a family is one of the two, and that it agrees with the
// address it was read from.
func checkFamily(t *testing.T, family, addr string) {
	t.Helper()
	if family != "" && family != "ipv4" && family != "ipv6" {
		t.Fatalf("family %q is not a family (from %q)", family, addr)
	}
	if family != "" && familyOf(addr) != "" && familyOf(addr) != family {
		t.Fatalf("family %q disagrees with address %q", family, addr)
	}
}

func FuzzParseDnsmasqLeases(f *testing.F) {
	seedFuzz(f, "dnsmasq.leases")
	f.Add("0 00:00:5e:00:53:01 192.0.2.5 * *")
	f.Fuzz(func(t *testing.T, raw string) {
		for _, l := range ParseDnsmasqLeases(raw, refNow) {
			checkLease(t, l)
		}
	})
}

func FuzzParseDnsmasqConf(f *testing.F) {
	seedFuzz(f, "dnsmasq.conf")
	f.Add("dhcp-range=192.0.2.50,192.0.2.150,12h\n")
	f.Add("dhcp-host=00:00:5e:00:53:01,192.0.2.10,host\n")
	f.Fuzz(func(t *testing.T, raw string) {
		pools, reservations := ParseDnsmasqConf("/etc/dnsmasq.conf", raw)
		for _, p := range pools {
			checkPool(t, p)
		}
		for _, r := range reservations {
			checkReservationParsed(t, r)
		}
	})
}

func FuzzParseKeaConfig(f *testing.F) {
	seedFuzz(f, "kea-dhcp4.conf")
	f.Add(`{"Dhcp4":{"subnet4":[{"subnet":"192.0.2.0/24","pools":[{"pool":"192.0.2.5 - 192.0.2.9"}]}]}}`)
	f.Fuzz(func(t *testing.T, raw string) {
		pools, reservations := ParseKeaConfig("/etc/kea/kea-dhcp4.conf", raw)
		for _, p := range pools {
			checkPool(t, p)
		}
		for _, r := range reservations {
			checkReservationParsed(t, r)
		}
	})
}

func FuzzParseKeaLeasesCSV(f *testing.F) {
	seedFuzz(f, "kea-leases4.csv")
	f.Add("address,hwaddr,expire,state\n192.0.2.5,00:00:5e:00:53:01,1893456000,0\n")
	f.Fuzz(func(t *testing.T, raw string) {
		for _, l := range ParseKeaLeasesCSV(raw, refNow) {
			checkLease(t, l)
		}
	})
}
