package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-network/internal/dhcp"
	"github.com/tui-tools/tui-network/internal/network"
)

// checkTimeout bounds the read. Loading the model shells out to networkctl,
// resolvectl and ip, and a machine whose network service is wedged must not
// hang a non-interactive check forever.
const checkTimeout = 30 * time.Second

// checkReport is what --check prints: the model the backend parsed, plus the
// counts a test can assert on without walking the whole structure.
//
// It is a report of the read path only. --check never builds and never runs a
// mutation: the whole point is that it is safe to run anywhere, including in
// CI against a production-shaped machine.
type checkReport struct {
	Tool    string `json:"tool"`
	Version string `json:"version"`
	Backend string `json:"backend"`
	// Describe is the backend's own one-line summary, which is where the demo
	// backend says it is a demo.
	Describe string `json:"describe"`
	// Running and ResolvedRunning report which daemons answered.
	Running         bool `json:"running"`
	ResolvedRunning bool `json:"resolvedRunning"`
	// ForeignManager names another network manager found on the machine.
	ForeignManager string `json:"foreignManager,omitempty"`
	// Links, Managed, Routes and ConfigFiles are the totals across the model.
	// Managed is the count a smoke test uses to tell a networkd machine from
	// a NetworkManager one without parsing the whole model.
	Links       int `json:"links"`
	Managed     int `json:"managed"`
	Routes      int `json:"routes"`
	ConfigFiles int `json:"configFiles"`
	// Compat is what the backend version probe found. It is reported rather
	// than asserted: an untested version is a fact about the machine, not a
	// failure of the read path.
	Compat compat.Result `json:"compat"`
	// Model is the parsed state in full.
	Model network.Model `json:"model"`
	// Gateways is the router's uplink view derived from the routing table:
	// the default routes, which one is the active default, and whether the
	// machine has more than one uplink to fail over between.
	Gateways gatewaysReport `json:"gateways"`
	// Dhcp is the router's DHCP server, read the same read-only way: which
	// server is present and active, and how many pools, reservations and leases
	// it has. It is its own block because it is its own backend.
	Dhcp dhcpReport `json:"dhcp"`
}

// gatewaysReport is the gateway half of --check: the counts a smoke test
// asserts on without walking the model, plus the derived list in full with its
// active flags.
type gatewaysReport struct {
	// Count is how many candidate uplinks (default-route gateways) were found.
	Count int `json:"count"`
	// Active is how many of them are the current default (the lowest-metric
	// route of their family, or every leg of a multipath default).
	Active int `json:"active"`
	// MultipleUplinks reports that more than one uplink exists, which is the
	// precondition for a failover.
	MultipleUplinks bool `json:"multipleUplinks"`
	// List is the derived gateways in full, in display order.
	List []network.Gateway `json:"list"`
}

// gatewaysCheck derives the uplink view from the parsed model. It reads
// nothing extra: the routing table Load already gathered is all the gateways
// are made of, so the reachability probe (an ip route get per gateway) is left
// to the interactive screen and never run under --check.
func gatewaysCheck(model network.Model) gatewaysReport {
	list := network.Gateways(model)
	report := gatewaysReport{
		Count:           len(list),
		MultipleUplinks: len(list) > 1,
		List:            list,
	}
	for _, gw := range list {
		if gw.Active {
			report.Active++
		}
	}
	return report
}

// dhcpReport is the DHCP half of --check: the counts a smoke test asserts on
// without walking the whole model, plus the parsed model in full.
type dhcpReport struct {
	// Backend is the detected server ("dnsmasq", "kea"), "none" when neither is
	// present.
	Backend string `json:"backend"`
	// Present and Active report the server's state.
	Present bool `json:"present"`
	Active  bool `json:"active"`
	// CombinedDNS reports that this server also answers DNS (dnsmasq does).
	CombinedDNS bool `json:"combinedDns"`
	// Pools, Reservations and Leases are the totals the model carries.
	Pools        int `json:"pools"`
	Reservations int `json:"reservations"`
	Leases       int `json:"leases"`
	// Compat is the DHCP server version probe.
	Compat compat.Result `json:"compat"`
	// Model is the parsed DHCP state in full.
	Model dhcp.Model `json:"model"`
}

// runCheck exercises the backend's real read path and prints the parsed model
// as JSON. It returns an error when the backend cannot be read, which main
// turns into a non-zero exit — so a caller can treat the exit code alone as
// the verdict.
//
// A machine running NetworkManager is not a failure: networkctl still lists
// the links, every one of them comes back unmanaged, and the report says so.
// That is the read path working, and it is what the smoke test asserts there.
func runCheck(backend network.Backend, backendCompat compat.Result,
	dhcpBackend dhcp.Backend, dhcpCompat compat.Result, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	model, err := backend.Load(ctx)
	if err != nil {
		return fmt.Errorf("%s backend read failed: %w", backend.Name(), err)
	}

	report := checkReport{
		Tool:            toolName,
		Version:         version,
		Backend:         backend.Name(),
		Describe:        backend.Describe(),
		Running:         model.Running,
		ResolvedRunning: model.ResolvedRunning,
		ForeignManager:  model.ForeignManager,
		Links:           len(model.Links),
		Routes:          len(model.Routes),
		ConfigFiles:     len(model.ConfigFiles),
		Compat:          backendCompat,
		Model:           model,
		Gateways:        gatewaysCheck(model),
		Dhcp:            dhcpCheck(ctx, dhcpBackend, dhcpCompat),
	}
	for _, link := range model.Links {
		if link.Managed {
			report.Managed++
		}
	}

	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

// dhcpCheck reads the DHCP server the read-only way and folds it into the
// report. A machine with no server is not a failure: the block says the server
// is absent, which is exactly what the smoke test asserts there.
func dhcpCheck(ctx context.Context, backend dhcp.Backend, dhcpCompat compat.Result) dhcpReport {
	model, err := backend.Load(ctx)
	report := dhcpReport{
		Backend: model.Server.Kind,
		Compat:  dhcpCompat,
		Model:   model,
	}
	if report.Backend == dhcp.KindNone {
		report.Backend = "none"
	}
	if err != nil {
		return report
	}
	report.Present = model.Server.Present
	report.Active = model.Server.Active
	report.CombinedDNS = model.Server.CombinedDNS
	report.Pools = len(model.Pools)
	report.Reservations = len(model.Reservations)
	report.Leases = len(model.Leases)
	return report
}
