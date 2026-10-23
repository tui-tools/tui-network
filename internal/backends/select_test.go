package backends

import "testing"

func TestDecide(t *testing.T) {
	both := Facts{NetworkdInstalled: true, NetworkManagerInstalled: true}
	cases := []struct {
		name    string
		facts   Facts
		want    string
		foreign string
		fails   bool
	}{
		{"only NetworkManager runs", with(both, func(f *Facts) { f.NetworkManagerActive = true }),
			NetworkManagerName, "", false},
		{"only networkd runs", with(both, func(f *Facts) { f.NetworkdActive = true }),
			NetworkdName, "", false},
		{"both, NetworkManager owns more", with(both, func(f *Facts) {
			f.NetworkdActive, f.NetworkManagerActive = true, true
			f.NetworkManagerLinks, f.NetworkdLinks = 2, 0
		}), NetworkManagerName, "systemd-networkd", false},
		{"both, networkd owns more", with(both, func(f *Facts) {
			f.NetworkdActive, f.NetworkManagerActive = true, true
			f.NetworkManagerLinks, f.NetworkdLinks = 1, 3
		}), NetworkdName, "NetworkManager", false},
		{"both, a tie keeps networkd", with(both, func(f *Facts) {
			f.NetworkdActive, f.NetworkManagerActive = true, true
		}), NetworkdName, "NetworkManager", false},
		{"neither runs, both installed", both, NetworkdName, "", false},
		{"neither runs, only nmcli", Facts{NetworkManagerInstalled: true},
			NetworkManagerName, "", false},
		{"nothing installed", Facts{}, "", "", true},
	}
	for _, c := range cases {
		got, err := Decide(c.facts)
		if c.fails {
			if err == nil {
				t.Errorf("%s: want an error", c.name)
			}
			continue
		}
		if err != nil || got.Name != c.want || got.Foreign != c.foreign || got.Detail == "" {
			t.Errorf("%s: got %+v, %v", c.name, got, err)
		}
	}
}

func with(f Facts, change func(*Facts)) Facts {
	change(&f)
	return f
}

func TestResolveByConfiguration(t *testing.T) {
	for value, want := range map[string]string{
		BackendNetworkd: NetworkdName, BackendNetworkManager: NetworkManagerName,
	} {
		got, err := Resolve(value)
		if err != nil || got.Name != want {
			t.Errorf("%s: %+v %v", value, got, err)
		}
	}
	if _, err := Resolve("wicked"); err == nil {
		t.Error("an unknown backend must be refused")
	}
}

func TestCounters(t *testing.T) {
	networkctl := "  1 lo     loopback carrier  unmanaged\n" +
		"  2 enp1s0 ether    routable configured\n" +
		"  3 wlan0  wlan     routable unmanaged\n"
	if n := CountNetworkdLinks(networkctl); n != 1 {
		t.Errorf("networkd links = %d", n)
	}
	nmcli := "eth1:ethernet:connected\ndummy0:dummy:disconnected\n" +
		"lo:loopback:connected (externally)\npeer1:ethernet:unmanaged\n" +
		"docker0:bridge:connected (externally)\n"
	if n := CountNetworkManagerLinks(nmcli); n != 2 {
		t.Errorf("NetworkManager links = %d", n)
	}
}
