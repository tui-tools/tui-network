package network

import "testing"

// twoUplinks is a model with an active wired default and a standby wireless
// one, the shape the Gateways screen and its failover are built for.
func twoUplinks() Model {
	return Model{
		Links: []Link{
			{Name: "wan0", Managed: true, NetworkFile: "/etc/systemd/network/10-wan0.network"},
			{Name: "wan1", Managed: false},
		},
		Routes: []Route{
			{Destination: "default", Gateway: "192.0.2.1", Link: "wan0",
				Metric: 100, Family: "ipv4", Protocol: "static"},
			{Destination: "default", Gateway: "198.51.100.1", Link: "wan1",
				Metric: 600, Family: "ipv4", Protocol: "dhcp"},
			{Destination: "192.0.2.0/24", Link: "wan0", Family: "ipv4"},
		},
		ConfigFiles: []ConfigFile{
			{Path: "/etc/systemd/network/10-wan0.network",
				Settings: []Setting{
					{Section: "Match", Key: "Name", Value: "wan0"},
					{Section: "Network", Key: "Gateway", Value: "192.0.2.1"},
				},
				MatchName: "wan0", Links: []string{"wan0"}},
		},
	}
}

func TestGatewaysDerivesUplinks(t *testing.T) {
	gws := Gateways(twoUplinks())
	if len(gws) != 2 {
		t.Fatalf("got %d gateways, want 2", len(gws))
	}
	// The lowest-metric default is active and sorts first.
	active := gws[0]
	if active.Interface != "wan0" || !active.Active {
		t.Errorf("active gateway = %+v, want wan0 active", active)
	}
	if !active.Managed || !active.Persistent ||
		active.ConfigFile != "/etc/systemd/network/10-wan0.network" {
		t.Errorf("wan0 annotations = %+v", active)
	}
	standby := gws[1]
	if standby.Interface != "wan1" || standby.Active {
		t.Errorf("standby gateway = %+v, want wan1 not active", standby)
	}
	if standby.Managed || standby.Persistent {
		t.Errorf("wan1 is unmanaged with no file, so it cannot be persistent: %+v", standby)
	}
}

func TestGatewaysExpandsMultipath(t *testing.T) {
	m := Model{
		Routes: []Route{
			{Destination: "default", Metric: 100, Family: "ipv4",
				NextHops: []NextHop{
					{Gateway: "192.0.2.1", Link: "wan0"},
					{Gateway: "198.51.100.1", Link: "wan1"},
				}},
		},
	}
	gws := Gateways(m)
	if len(gws) != 2 {
		t.Fatalf("got %d gateways, want one per leg", len(gws))
	}
	for _, gw := range gws {
		if !gw.Active || !gw.Multipath {
			t.Errorf("a leg of the active multipath default must be active and multipath: %+v", gw)
		}
	}
}

func TestGatewaysDropsGatewaylessDefault(t *testing.T) {
	// A default route with no gateway (a link-only default) is not an uplink to
	// switch to, so it is not listed.
	m := Model{Routes: []Route{
		{Destination: "default", Link: "wan0", Family: "ipv4"},
	}}
	if gws := Gateways(m); len(gws) != 0 {
		t.Errorf("a gatewayless default became %d uplinks", len(gws))
	}
}

func TestPromoteMetricWinsTheRace(t *testing.T) {
	gws := Gateways(twoUplinks())
	standby, ok := Standby(gws)
	if !ok || standby.Interface != "wan1" {
		t.Fatalf("Standby = %+v, %v, want wan1", standby, ok)
	}
	// Promoting the standby must give it a metric below the active one (100),
	// so the kernel's lowest-metric rule makes it the default.
	metric := PromoteMetric(gws, standby)
	if metric != 99 {
		t.Errorf("PromoteMetric = %d, want 99 (one below the active 100)", metric)
	}
}

func TestPromoteMetricFloorsAtZero(t *testing.T) {
	gws := []Gateway{
		{Interface: "wan0", Address: "192.0.2.1", Metric: 0, Family: "ipv4"},
		{Interface: "wan1", Address: "198.51.100.1", Metric: 100, Family: "ipv4"},
	}
	if got := PromoteMetric(gws, gws[1]); got != 0 {
		t.Errorf("PromoteMetric floored = %d, want 0", got)
	}
}

func TestStandbyRespectsFamily(t *testing.T) {
	// The active default is IPv4; an IPv6 default is not a failover target for
	// it, because it does not carry the same traffic.
	m := Model{Routes: []Route{
		{Destination: "default", Gateway: "192.0.2.1", Link: "wan0", Metric: 100, Family: "ipv4"},
		{Destination: "default", Gateway: "2001:db8::1", Link: "wan0", Metric: 100, Family: "ipv6"},
	}}
	gws := Gateways(m)
	if _, ok := Standby(gws); ok {
		t.Errorf("an IPv6 default must not be a standby for the IPv4 active")
	}
}
