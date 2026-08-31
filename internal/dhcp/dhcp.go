// Package dhcp defines the backend-neutral model tui-network's DHCP screen
// renders and the interface a DHCP server implementation satisfies. It is to
// the DHCP screen what internal/network is to the links screen: the UI knows
// only these types and never builds a dnsmasq or Kea command itself. Mutations
// are Command values produced by a backend, shown in a preview dialog and only
// then executed.
//
// A router needs a DHCP server and local DNS for its LAN. tui-network already
// owns the machine's links, addresses, routes and what systemd-resolved
// reports; this package adds the piece a router profile also needs — the pool
// the server hands out, the reservations that pin a host to an address, and the
// leases it has granted — for the two servers a small router runs: dnsmasq
// (which serves DNS and DHCP together) and ISC Kea.
package dhcp

import (
	"context"

	"github.com/tui-tools/tui-kit/runner"
)

// Command is a single privileged invocation the user is about to run. It is the
// same alias the links screen uses, so a backend hands the very value the
// confirm dialog displayed straight to the kit runner.
type Command = runner.Command

// The DHCP servers this tool knows how to read. The value is what the manifest
// backend block and the compat probe are keyed on.
const (
	// KindNone is a machine with neither server present.
	KindNone = ""
	// KindDnsmasq is dnsmasq, which serves DNS and DHCP from one process.
	KindDnsmasq = "dnsmasq"
	// KindKea is ISC Kea, the DHCP server the ISC ships in place of the old
	// dhcpd.
	KindKea = "kea"
)

// Lease is one address a server has handed out, as its lease file records it.
// Every field but the address is optional: a client that offered no hostname
// and no client id still has a lease.
type Lease struct {
	// MAC is the client hardware address, the lease's primary identifier.
	MAC string
	// IP is the leased address.
	IP string
	// Hostname is the name the client offered, empty when it offered none.
	Hostname string
	// ClientID is the DHCP client identifier, when the server recorded one.
	ClientID string
	// Expiry is the lease expiry already rendered as human text ("in 47m",
	// "expired 2m ago", "never"): the two servers store the clock in different
	// units, so the backend renders it rather than the UI.
	Expiry string
	// Family is "ipv4" or "ipv6".
	Family string
}

// Pool is one range of addresses a server hands out — a dnsmasq `dhcp-range` or
// a Kea subnet pool.
type Pool struct {
	// Name labels the pool: the dnsmasq tag or interface it is scoped to, or
	// the Kea subnet it belongs to. Empty on an unscoped range.
	Name string
	// Start and End are the first and last address of the range.
	Start string
	// End is the last address of the range.
	End string
	// Netmask is the IPv4 mask a dnsmasq range may carry, empty when none.
	Netmask string
	// PrefixLen is the IPv6 prefix length, zero on an IPv4 pool.
	PrefixLen int
	// LeaseTime is the lease duration as written ("12h", "infinite"), empty
	// when the server was left to its default.
	LeaseTime string
	// Family is "ipv4" or "ipv6".
	Family string
	// Mode is a dnsmasq range mode that is not a plain pool ("static",
	// "ra-only", "ra-stateless", "ra-names"), empty for an ordinary range.
	Mode string
	// Source is the file the range was read from, which is the file a range
	// edit rewrites.
	Source string
}

// Reservation pins a client to a fixed address — a dnsmasq `dhcp-host` or a Kea
// host reservation.
type Reservation struct {
	// MAC is the client hardware address the reservation matches on. Either
	// this or ClientID identifies the client.
	MAC string
	// IP is the address the client is pinned to, empty on a reservation that
	// only names the host or sets a tag.
	IP string
	// Hostname is the name assigned to the client, empty when none.
	Hostname string
	// ClientID is the `id:` form dnsmasq also matches on, empty when the
	// reservation matches by MAC.
	ClientID string
	// Family is "ipv4" or "ipv6".
	Family string
	// Source is the file the reservation was read from, which is the file a
	// removal rewrites. A reservation the tool itself would add carries the
	// managed file here.
	Source string
}

// Server is what was found about the DHCP server on this machine.
type Server struct {
	// Kind is one of the Kind* constants, KindNone when neither server is
	// present.
	Kind string
	// Version is the server version the probe read, empty when unknown.
	Version string
	// Present reports that the server is installed (its binary or its
	// configuration was found).
	Present bool
	// Active reports that the server service is running.
	Active bool
	// CombinedDNS reports that this server also answers DNS, which dnsmasq does
	// and Kea does not. The screen says so, because on a dnsmasq router the DNS
	// the links screen reads from systemd-resolved is not the whole story.
	CombinedDNS bool
	// ConfPaths are the configuration files that were read, in read order.
	ConfPaths []string
	// LeasesPath is the lease file that was read, empty when none was found.
	LeasesPath string
	// ManagedFile is the file tui-network writes reservations to, so an added
	// reservation never rewrites a file the administrator hand-maintains.
	ManagedFile string
	// Explain is one sentence for the empty state: why the screen has nothing
	// to show — no server installed, or one installed but not running.
	Explain string
}

// Model is the whole picture the DHCP screen renders.
type Model struct {
	Server       Server
	Pools        []Pool
	Reservations []Reservation
	Leases       []Lease
}

// Capabilities tells the UI which mutations a backend offers, so the key map is
// built from the backend rather than hardcoded. A read-only backend (Kea in
// phase one, or a machine with no server) reports all false.
type Capabilities struct {
	// SupportsAddReservation reports whether a static reservation can be added.
	SupportsAddReservation bool
	// SupportsRemoveReservation reports whether a reservation can be removed.
	SupportsRemoveReservation bool
	// SupportsSetPoolRange reports whether a pool's range can be adjusted.
	SupportsSetPoolRange bool
	// ManagedFile is where an added reservation lands, shown in the UI so the
	// user knows which file a change will touch.
	ManagedFile string
}

// WritePlan is a configuration change the user is about to make: what the file
// will look like, how that differs from what is there now, and the exact
// commands that apply it. It mirrors the links screen's plan for a .network
// file: nothing is installed until the commands run.
type WritePlan struct {
	// Path is the destination file.
	Path string
	// Content is the text that will be installed.
	Content string
	// Diff is the unified diff against the current file.
	Diff string
	// TempPath is the staging file the install command copies from.
	TempPath string
	// Commands are run in order, and are what the confirm dialog shows: install
	// the staged file, then reload or restart the server.
	Commands []Command
}

// Backend is the boundary between the DHCP screen and the machine. Load reads
// state; the Build* methods turn user intent into a previewable WritePlan; Run
// executes a Command the user confirmed. Nothing else may mutate the system.
type Backend interface {
	// Name identifies the backend for the header and the compat probe.
	Name() string
	// Describe is the one-line summary shown in the header.
	Describe() string
	// Capabilities reports which mutations this backend offers.
	Capabilities() Capabilities
	// Preview renders the exact command line Run will execute.
	Preview(cmd Command) string
	// Load reads the server, its pools, its reservations and its leases.
	Load(ctx context.Context) (Model, error)
	// Run executes a previously previewed command.
	Run(ctx context.Context, cmd Command) (string, error)

	// BuildAddReservation renders the file that adds a static reservation and
	// returns the diff plus the commands that install and reload it.
	BuildAddReservation(r Reservation) (WritePlan, error)
	// BuildRemoveReservation renders the file with a reservation removed.
	BuildRemoveReservation(r Reservation) (WritePlan, error)
	// BuildSetPoolRange renders the file with an existing pool's range adjusted
	// to newStart..newEnd. orig identifies the pool to change, and the file it
	// lives in, by the range it has today.
	BuildSetPoolRange(orig Pool, newStart, newEnd string) (WritePlan, error)
}
