// Package network defines the backend-agnostic model tui-network renders and
// the interface every network implementation satisfies. The UI knows only
// these types: it never builds a networkctl, resolvectl or ip argv itself.
// Mutations are Command values produced by the backend, shown in a preview
// dialog and only then executed.
package network

import (
	"context"
	"strconv"
	"strings"

	"github.com/tui-tools/tui-kit/runner"
)

// Command is a single privileged invocation the user is about to run. Argv
// excludes any privilege wrapper: the backend adds it when previewing and when
// executing.
//
// It is an alias rather than a type of its own, so a backend hands the very
// value the confirm dialog displayed straight to the kit runner, with no
// conversion in between. That identity is what makes the preview a promise.
type Command = runner.Command

// Setup states are the values systemd-networkd reports as a link's
// administrative state. They are strings rather than an enum because the list
// grows with systemd and an unknown one must still be shown.
const (
	// SetupUnmanaged is a link networkd does not configure — because no
	// .network file matches it, or because NetworkManager owns it.
	SetupUnmanaged = "unmanaged"
	// SetupConfigured is a link networkd has finished configuring.
	SetupConfigured = "configured"
	// SetupConfiguring is a link networkd is still working on.
	SetupConfiguring = "configuring"
	// SetupPending is a link networkd has not started on yet.
	SetupPending = "pending"
	// SetupFailed is a link whose configuration failed.
	SetupFailed = "failed"
)

// Address is one address configured on a link.
type Address struct {
	// Address is the textual form ("192.0.2.98", "fe80::ff:fe00:4").
	Address string
	// Prefix is the prefix length in bits.
	Prefix int
	// Family is "ipv4" or "ipv6".
	Family string
	// Source is where the address came from: "static", "DHCPv4", "DHCPv6",
	// "foreign" (configured by something other than networkd), "RA".
	Source string
	// Scope is "global", "link" or "host".
	Scope string
	// Provider is the server that handed the address out, when there is one.
	Provider string
}

// String renders the address in CIDR form.
func (a Address) String() string {
	if a.Prefix <= 0 {
		return a.Address
	}
	return a.Address + "/" + strconv.Itoa(a.Prefix)
}

// Route is one entry of the kernel routing table.
type Route struct {
	// Destination is "default" or a prefix in CIDR form.
	Destination string
	Gateway     string
	// Link is the interface name the route is attached to.
	Link string
	// Source is the preferred source address, when the kernel reports one.
	Source string
	// Protocol is who installed the route ("kernel", "dhcp", "static").
	Protocol string
	Scope    string
	Metric   int
	// Family is "ipv4" or "ipv6".
	Family string
	// Table is the routing table name, empty for the main table.
	Table string
}

// DHCP is what a link's dynamic configuration lease looks like. Every field is
// optional: a static link carries none of them.
type DHCP struct {
	// Enabled reports that the link has a DHCP-sourced address.
	Enabled bool
	// Server is the address of the server that granted the lease.
	Server string
	// Address is the leased address.
	Address string
	// ClientID and DUID are the identifiers networkd presents.
	ClientID string
	DUID     string
	// LeaseTimestamp, Timeout1 and Timeout2 are the lease clock as
	// systemd reports it, already rendered as human text.
	LeaseTimestamp string
	Timeout1       string
	Timeout2       string
}

// Link is one network interface, in backend-neutral terms.
type Link struct {
	Index int
	Name  string
	// Type is the kernel link type ("ether", "wlan", "loopback").
	Type string
	// Kind is the virtual device kind ("veth", "bridge"), empty for physical
	// interfaces.
	Kind string
	// Setup is the administrative state (see the Setup* constants).
	Setup string
	// Operational is the operational state ("routable", "degraded",
	// "carrier", "no-carrier", "off").
	Operational string
	// Carrier is the carrier state, when the backend reports it separately.
	Carrier string
	// Online is the online state ("online", "offline", "partial").
	Online string
	// MAC is the hardware address, empty on a link that has none.
	MAC string
	MTU int
	// Driver is the kernel driver, when it is known.
	Driver string

	Addresses []Address
	Gateways  []string
	// DNS and SearchDomains are what this link contributes to resolution.
	DNS           []string
	SearchDomains []string

	// NetworkFile is the .network file networkd matched, empty when none did.
	NetworkFile string
	// NetworkFileDropins are the drop-in files that also apply.
	NetworkFileDropins []string

	DHCP DHCP

	// Managed reports whether tui-network may change this link. A link
	// NetworkManager owns, or one networkd calls unmanaged, is read-only.
	Managed bool
	// ReadOnlyReason explains, in one sentence, why Managed is false.
	ReadOnlyReason string
}

// Label renders the link for a one-line summary.
func (l Link) Label() string {
	if l.Kind != "" && l.Kind != l.Type {
		return l.Name + " (" + l.Kind + ")"
	}
	return l.Name
}

// PrimaryAddress is the first global address, which is the one worth showing
// in a list. It returns an empty string on a link that has none.
func (l Link) PrimaryAddress() string {
	for _, a := range l.Addresses {
		if a.Scope == "global" && a.Family == "ipv4" {
			return a.String()
		}
	}
	for _, a := range l.Addresses {
		if a.Scope == "global" {
			return a.String()
		}
	}
	return ""
}

// Gateway is the first gateway of the link, or an empty string.
func (l Link) Gateway() string {
	if len(l.Gateways) == 0 {
		return ""
	}
	return l.Gateways[0]
}

// Setting is one `Key=Value` line of a .network file, in the section it
// belongs to.
type Setting struct {
	Section string
	Key     string
	Value   string
}

// ConfigFile is a systemd .network file as it is on disk: the raw text, the
// settings parsed out of it, and which links it matches.
type ConfigFile struct {
	// Path is the absolute path of the file.
	Path string
	// Raw is the file's text, shown read-only in the detail view.
	Raw string
	// Settings are the parsed key/value lines, in file order.
	Settings []Setting
	// Links are the names of the links networkd reported this file for.
	Links []string
	// MatchName is the `[Match] Name=` value, which is what the guided form
	// edits.
	MatchName string
}

// Get returns the value of a setting, and whether it was present.
func (c ConfigFile) Get(section, key string) (string, bool) {
	for _, s := range c.Settings {
		if strings.EqualFold(s.Section, section) && strings.EqualFold(s.Key, key) {
			return s.Value, true
		}
	}
	return "", false
}

// All returns every value of a setting that may legally repeat (DNS=, Address=).
func (c ConfigFile) All(section, key string) []string {
	var out []string
	for _, s := range c.Settings {
		if strings.EqualFold(s.Section, section) && strings.EqualFold(s.Key, key) {
			out = append(out, s.Value)
		}
	}
	return out
}

// Model is the whole picture tui-network renders.
type Model struct {
	// Backend names the implementation that produced this model.
	Backend string
	// Running reports whether the network manager itself is up. A machine
	// where it is not is shown read-only rather than empty.
	Running bool
	// ResolvedRunning reports whether systemd-resolved answered.
	ResolvedRunning bool
	// ForeignManager names another manager that owns the machine's links
	// ("NetworkManager"), empty when none was detected.
	ForeignManager string

	Links  []Link
	Routes []Route
	// GlobalDNS and GlobalSearchDomains are the resolver settings that are
	// not attached to a link.
	GlobalDNS           []string
	GlobalSearchDomains []string
	// ConfigFiles are the .network files found in the search paths.
	ConfigFiles []ConfigFile
}

// Link returns the link with the given name.
func (m Model) Link(name string) (Link, bool) {
	for _, l := range m.Links {
		if l.Name == name {
			return l, true
		}
	}
	return Link{}, false
}

// RoutesFor returns the routes attached to one link.
func (m Model) RoutesFor(name string) []Route {
	var out []Route
	for _, r := range m.Routes {
		if r.Link == name {
			out = append(out, r)
		}
	}
	return out
}

// ConfigFor returns the .network file that applies to a link: the one networkd
// itself reported, falling back to a file whose [Match] Name covers the link.
func (m Model) ConfigFor(l Link) (ConfigFile, bool) {
	for _, c := range m.ConfigFiles {
		if c.Path == l.NetworkFile {
			return c, true
		}
	}
	for _, c := range m.ConfigFiles {
		for _, name := range c.Links {
			if name == l.Name {
				return c, true
			}
		}
	}
	return ConfigFile{}, false
}

// FileSpec describes the .network file the guided form wants written. The
// backend renders it into file text and into the commands that install it.
type FileSpec struct {
	// Path is the absolute destination, in the networkd configuration
	// directory.
	Path string
	// MatchName is the `[Match] Name=` value.
	MatchName string
	// DHCP is "yes", "ipv4", "ipv6" or "no".
	DHCP string
	// Address, Gateway and DNS are the static settings, used when DHCP does
	// not cover the family. Address is CIDR; DNS may hold several servers.
	Address string
	Gateway string
	DNS     []string
	// Domains is the search domain list.
	Domains []string
}

// Capabilities tells the UI what a backend supports, so the key map and the
// forms are built from the backend rather than hardcoded.
type Capabilities struct {
	// DHCPModes are the accepted values of FileSpec.DHCP.
	DHCPModes []string
	// SupportsUpDown reports whether links can be brought up and down.
	SupportsUpDown bool
	// SupportsRenew reports whether a dynamic lease can be renewed.
	SupportsRenew bool
	// SupportsFlushCaches reports whether the resolver cache can be flushed.
	SupportsFlushCaches bool
	// SupportsRuntimeDNS reports whether DNS servers and search domains can
	// be set on a link at runtime.
	SupportsRuntimeDNS bool
	// SupportsFileEdit reports whether the guided .network editor is offered.
	SupportsFileEdit bool
	// ConfigDir is where a written .network file lands.
	ConfigDir string
}

// Backend is the boundary between the UI and the machine. Load reads state;
// the Build* methods turn user intent into previewable Commands; Run executes
// a Command the user confirmed. Nothing else may mutate the system.
type Backend interface {
	// Name is the backend identifier ("systemd-networkd", "demo").
	Name() string
	// Describe is the one-line summary shown in the header.
	Describe() string
	// Capabilities reports what this backend supports.
	Capabilities() Capabilities

	// Preview renders the exact command line Run will execute, privilege
	// wrapper included. This is the text shown in the confirm dialog.
	Preview(cmd Command) string

	// Load reads the current network state.
	Load(ctx context.Context) (Model, error)
	// LoadLink re-reads one link in full, which is what the detail screen
	// shows: a link's own DNS, search domains and lease live in the
	// per-link read rather than in the list.
	LoadLink(ctx context.Context, name string) (Link, error)
	// Journal returns the manager's recent log lines mentioning a link.
	Journal(ctx context.Context, link string) ([]string, error)
	// Run executes a previously previewed command.
	Run(ctx context.Context, cmd Command) (string, error)

	// BuildLinkAction builds up, down, reconfigure or renew for a link.
	BuildLinkAction(action, link string) (Command, error)
	// BuildReload re-reads the .network and .netdev files.
	BuildReload() (Command, error)
	// BuildFlushCaches empties the resolver cache.
	BuildFlushCaches() (Command, error)
	// BuildSetDNS sets the DNS servers of a link at runtime.
	BuildSetDNS(link string, servers []string) (Command, error)
	// BuildSetDomains sets the search domains of a link at runtime.
	BuildSetDomains(link string, domains []string) (Command, error)
	// BuildWriteConfig renders a .network file, writes it to a temporary
	// path and returns the diff against what is on disk today plus the
	// commands that install it and reload the manager. Nothing is installed
	// until those commands are run.
	BuildWriteConfig(spec FileSpec) (WritePlan, error)
}

// WritePlan is a file change the user is about to make: what the file will
// look like, how that differs from what is there now, and the exact commands
// that apply it.
type WritePlan struct {
	// Path is the destination file.
	Path string
	// Content is the text that will be installed.
	Content string
	// Diff is the unified diff against the current file, empty when nothing
	// would change.
	Diff string
	// TempPath is the staging file the install command copies from.
	TempPath string
	// Commands are run in order, and are what the confirm dialog shows.
	Commands []Command
}

// The link actions BuildLinkAction accepts.
const (
	ActionUp          = "up"
	ActionDown        = "down"
	ActionReconfigure = "reconfigure"
	ActionRenew       = "renew"
)
