package dhcpd

import (
	"strings"
	"testing"
)

func TestParseKeaConfig(t *testing.T) {
	raw := mustReadFixture(t, "kea-dhcp4.conf")
	pools, reservations := ParseKeaConfig("/etc/kea/kea-dhcp4.conf", raw)

	if len(pools) != 1 {
		t.Fatalf("got %d pools, want 1", len(pools))
	}
	pool := pools[0]
	if pool.Start != "192.0.2.50" || pool.End != "192.0.2.150" {
		t.Errorf("pool range parsed wrong: %+v", pool)
	}
	if pool.Name != "192.0.2.0/24" {
		t.Errorf("pool should carry its subnet as its name: %+v", pool)
	}
	// The subnet's own valid-lifetime wins over the service default.
	if pool.LeaseTime != "2h0m0s" {
		t.Errorf("pool lease time = %q, want the subnet's 7200s", pool.LeaseTime)
	}

	// One reservation in the subnet, one at the service level.
	if len(reservations) != 2 {
		t.Fatalf("got %d reservations, want 2", len(reservations))
	}
	if reservations[0].MAC != "00:00:5e:00:53:0f" || reservations[0].IP != "192.0.2.5" {
		t.Errorf("global reservation parsed wrong: %+v", reservations[0])
	}
	if reservations[1].Hostname != "printer" {
		t.Errorf("subnet reservation parsed wrong: %+v", reservations[1])
	}
}

func TestParseKeaLeasesCSV(t *testing.T) {
	raw := mustReadFixture(t, "kea-leases4.csv")
	leases := ParseKeaLeasesCSV(raw, refNow)

	// Three data rows, but the last is state 2 (expired-reclaimed) and dropped.
	if len(leases) != 2 {
		t.Fatalf("got %d leases, want 2 (the reclaimed one dropped): %+v", len(leases), leases)
	}
	if leases[0].IP != "192.0.2.50" || leases[0].MAC != "00:00:5e:00:53:01" ||
		leases[0].Hostname != "laptop" {
		t.Errorf("first lease parsed wrong: %+v", leases[0])
	}
	if !strings.HasPrefix(leases[0].Expiry, "in ") {
		t.Errorf("a future lease should read as remaining time, got %q", leases[0].Expiry)
	}
	if leases[1].ClientID != "01:00:00:5e:00:53:02" {
		t.Errorf("client id column not read: %+v", leases[1])
	}
}

func TestStripJSONComments(t *testing.T) {
	in := `{
  "a": "http://x", // trailing
  /* block
     spanning */
  "b": 1 # hash
}`
	out := stripJSONComments(in)
	// The // inside the string value must survive.
	if !strings.Contains(out, `"http://x"`) {
		t.Errorf("a // inside a string was stripped:\n%s", out)
	}
	if strings.Contains(out, "trailing") || strings.Contains(out, "block") ||
		strings.Contains(out, "hash") {
		t.Errorf("a comment survived stripping:\n%s", out)
	}
}
