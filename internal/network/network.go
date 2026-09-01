// Package network defines the backend-agnostic model tui-network renders and
// the interface every network implementation satisfies. The UI knows only
// these types: it never builds a networkctl, resolvectl or ip argv itself.
// Mutations are Command values produced by the backend, shown in a preview
// dialog and only then executed.
package network

import (
	"context"
	"sort"
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
	// NextHops are the legs of a multipath route, empty for a single-path
	// route. A multipath default route is several uplinks the kernel balances
	// across, so the Gateways view expands each leg into its own candidate.
	NextHops []NextHop
}

// NextHop is one leg of a multipath route: the gateway to send to and the link
// to send it over.
type NextHop struct {
	Gateway string
	Link    string
	// Weight is the leg's share of a multipath route, zero when unweighted.
	Weight int
}

// Egress is what `ip route get` resolves for a destination: which link the
// kernel would send over, through which gateway, from which source address. It
// is the read-only reachability probe the Gateways screen uses — a route
// lookup, never a packet.
type Egress struct {
	// Dev is the link the kernel would send over, empty when it could not
	// resolve one.
	Dev string
	// Gateway is the next hop, empty for an on-link destination.
	Gateway string
	// Source is the preferred source address the kernel would use.
	Source string
}

// Found reports whether the lookup resolved a link at all.
func (e Egress) Found() bool { return e.Dev != "" }

// Gateway is one candidate uplink: a default route's gateway, the link it
// leaves by, its priority, and whether it is the one the kernel is using right
// now. It is derived from the routing table (and the .network files behind it),
// not read as its own thing.
type Gateway struct {
	// Interface is the link the default route leaves by.
	Interface string
	// Address is the gateway (next hop) address.
	Address string
	// Metric is the route's priority: the lowest metric per family is the one
	// the kernel uses.
	Metric int
	// Family is "ipv4" or "ipv6".
	Family string
	// Protocol is who installed the default route ("dhcp", "static", "boot").
	Protocol string
	// Active reports that this gateway is (part of) the default the kernel is
	// using now: the lowest-metric default route of its family.
	Active bool
	// Multipath reports that this gateway is one leg of a multipath default
	// route, which the kernel load-balances rather than fails over.
	Multipath bool
	// Managed reports that systemd-networkd owns the interface, which is what
	// decides whether a persistent drop-in can be written for it.
	Managed bool
	// Persistent reports that a .network file sets a Gateway= for this
	// interface, so the priority survives a reconfigure.
	Persistent bool
	// ConfigFile is the .network file that configures the interface, when one
	// was found — where a persistent drop-in attaches.
	ConfigFile string
	// Egress is what `ip route get` said about reaching this gateway, when the
	// optional reachability probe ran. Dev empty means it was not probed or did
	// not resolve.
	Egress Egress
}

// Reachable reports that the reachability probe resolved this gateway to its
// own interface, which is the read that a gateway is directly reachable.
func (g Gateway) Reachable() bool {
	return g.Egress.Dev != "" && g.Egress.Dev == g.Interface
}

// Gateways derives the candidate uplinks from the model's routing table: every
// default route that carries a gateway (a multipath route once per leg),
// cross-referenced with the links and their .network files. The lowest-metric
// default route of each family is flagged Active, which is the one the kernel
// is using.
//
// It is a pure read of what Load already gathered: no command runs here, so the
// same derivation serves the screen, --check and the tests.
func Gateways(m Model) []Gateway {
	// The lowest metric seen per family decides which entries are active.
	minMetric := map[string]int{}
	seen := map[string]bool{}
	for _, r := range m.Routes {
		if r.Destination != "default" {
			continue
		}
		for _, family := range []string{r.Family} {
			if _, ok := minMetric[family]; !ok || r.Metric < minMetric[family] {
				if hasGateway(r) {
					minMetric[family] = r.Metric
					seen[family] = true
				}
			}
		}
	}

	var gws []Gateway
	for _, r := range m.Routes {
		if r.Destination != "default" {
			continue
		}
		for _, leg := range routeLegs(r) {
			gw := Gateway{
				Interface: leg.Link,
				Address:   leg.Gateway,
				Metric:    r.Metric,
				Family:    r.Family,
				Protocol:  r.Protocol,
				Multipath: len(r.NextHops) > 0,
			}
			if seen[r.Family] && r.Metric == minMetric[r.Family] {
				gw.Active = true
			}
			m.annotateGateway(&gw)
			gws = append(gws, gw)
		}
	}
	sortGateways(gws)
	return gws
}

// hasGateway reports whether a default route names a gateway, directly or
// through a multipath leg. A default route with no gateway at all (a link-only
// default) is not a candidate uplink.
func hasGateway(r Route) bool {
	if r.Gateway != "" {
		return true
	}
	for _, nh := range r.NextHops {
		if nh.Gateway != "" {
			return true
		}
	}
	return false
}

// routeLegs returns the (gateway, link) pairs a default route contributes: its
// single hop, or one per multipath leg. Legs with no gateway are dropped.
func routeLegs(r Route) []NextHop {
	if len(r.NextHops) > 0 {
		var legs []NextHop
		for _, nh := range r.NextHops {
			if nh.Gateway != "" {
				legs = append(legs, nh)
			}
		}
		return legs
	}
	if r.Gateway == "" {
		return nil
	}
	return []NextHop{{Gateway: r.Gateway, Link: r.Link}}
}

// annotateGateway fills in the facts that come from the links and their
// .network files rather than from the routing table: whether networkd manages
// the interface, and whether a file already sets a gateway for it.
func (m Model) annotateGateway(gw *Gateway) {
	link, ok := m.Link(gw.Interface)
	if !ok {
		return
	}
	gw.Managed = link.Managed
	file, ok := m.ConfigFor(link)
	if !ok {
		return
	}
	gw.ConfigFile = file.Path
	if _, ok := file.Get("Network", "Gateway"); ok {
		gw.Persistent = true
	}
	if _, ok := file.Get("Route", "Gateway"); ok {
		gw.Persistent = true
	}
}

// sortGateways orders the uplinks for display: active first, then by family,
// then by metric, then by interface — so the one in use is on top and the
// standbys follow in priority order.
func sortGateways(gws []Gateway) {
	sort.SliceStable(gws, func(i, j int) bool {
		a, b := gws[i], gws[j]
		if a.Active != b.Active {
			return a.Active
		}
		if a.Family != b.Family {
			return a.Family < b.Family
		}
		if a.Metric != b.Metric {
			return a.Metric < b.Metric
		}
		return a.Interface < b.Interface
	})
}

// PromoteMetric returns a route metric that makes gw the active default among
// gws: one below the lowest metric of the other default routes of the same
// family, floored at zero. It is the number the "set default" and "failover"
// commands write, so the chosen uplink wins the kernel's lowest-metric race.
func PromoteMetric(gws []Gateway, gw Gateway) int {
	lowestOther := -1
	for _, other := range gws {
		if other.Family != gw.Family {
			continue
		}
		if other.Interface == gw.Interface && other.Address == gw.Address {
			continue
		}
		if lowestOther < 0 || other.Metric < lowestOther {
			lowestOther = other.Metric
		}
	}
	if lowestOther <= 0 {
		return 0
	}
	return lowestOther - 1
}

// Standby returns the highest-priority gateway that is not currently active,
// for the family of the active default — the uplink a manual failover promotes.
func Standby(gws []Gateway) (Gateway, bool) {
	activeFamily := ""
	for _, gw := range gws {
		if gw.Active {
			activeFamily = gw.Family
			break
		}
	}
	best, found := Gateway{}, false
	for _, gw := range gws {
		if gw.Active || gw.Address == "" {
			continue
		}
		if activeFamily != "" && gw.Family != activeFamily {
			continue
		}
		if !found || gw.Metric < best.Metric {
			best, found = gw, true
		}
	}
	return best, found
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

// The virtual device kinds tui-network can create. They are the two a router
// actually needs: a VLAN rides on one parent link, a bridge gathers several.
const (
	NetdevVLAN   = "vlan"
	NetdevBridge = "bridge"
)

// NetdevKinds are those kinds in the order the picker offers them.
var NetdevKinds = []string{NetdevVLAN, NetdevBridge}

// The 802.1Q VLAN id range. Zero means "no VLAN" and 4095 is reserved, so
// neither is a device an operator can ask for.
const (
	MinVLANID = 1
	MaxVLANID = 4094
)

// NetdevFile is a systemd .netdev unit as it is on disk: the raw text, the
// settings parsed out of it, and what the [NetDev] section declares.
type NetdevFile struct {
	// Path is the absolute path of the file.
	Path string
	// Raw is the file's text.
	Raw string
	// Settings are the parsed key/value lines, in file order.
	Settings []Setting
	// Name is the `[NetDev] Name=` value, which is the device's name.
	Name string
	// Kind is the `[NetDev] Kind=` value, lower-cased ("vlan", "bridge").
	Kind string
	// Owned reports that tui-network wrote this file. Only an owned unit may
	// be removed from the TUI: a unit somebody else wrote is theirs.
	Owned bool
}

// NetdevSpec describes a virtual device the user asked for. The backend turns
// it into the .netdev unit and the member `.network` lines that make the device
// real, and refuses a spec that names something the machine cannot give it.
type NetdevSpec struct {
	// Kind is NetdevVLAN or NetdevBridge.
	Kind string
	// Name is the device's name, and the name the .netdev unit is written as.
	Name string
	// Parent is the link a VLAN rides on. It is unused by a bridge.
	Parent string
	// VLANID is the 802.1Q id, between MinVLANID and MaxVLANID.
	VLANID int
	// Members are the links a bridge gathers. They are unused by a VLAN.
	Members []string
}

// MemberLinks are the links a spec touches beyond the new device itself: a
// VLAN's parent, or a bridge's members. They are the links that get a member
// line written into their .network file — and the links whose session a
// re-parenting can drop.
func (s NetdevSpec) MemberLinks() []string {
	if s.Kind == NetdevVLAN {
		if s.Parent == "" {
			return nil
		}
		return []string{s.Parent}
	}
	return s.Members
}

// FileChange is one file of a multi-file plan: what it holds today, what it
// will hold, and whether it is being removed outright. A VLAN or a bridge is
// always more than one file — the unit plus every member's .network — so the
// dialog shows them as one diff and applies them as one confirmed plan.
type FileChange struct {
	// Path is the destination file.
	Path string
	// Before is what the destination holds today, empty when it does not exist.
	Before string
	// Content is the text that will be installed, empty for a removal.
	Content string
	// Remove reports that the file is deleted rather than written.
	Remove bool
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
	// NetdevFiles are the .netdev units found in the same search paths: the
	// virtual devices this machine declares.
	NetdevFiles []NetdevFile
}

// Netdev returns the .netdev unit that declares a device by name.
func (m Model) Netdev(name string) (NetdevFile, bool) {
	for _, unit := range m.NetdevFiles {
		if unit.Name == name {
			return unit, true
		}
	}
	return NetdevFile{}, false
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
	// SupportsNetdev reports whether VLANs and bridges can be created and
	// removed as .netdev units.
	SupportsNetdev bool
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

	// BuildCreateNetdev renders the .netdev unit for a VLAN or a bridge and
	// the member lines that attach it to the links it rides on, and returns
	// them as one multi-file plan: one diff over every file, then an install
	// per file and a single reload.
	//
	// The model is a parameter rather than backend state because every refusal
	// this builder makes is a question about the machine as it was last read —
	// does this name already exist, is the parent a link networkd manages,
	// which .network file carries a member — and the UI is holding that read.
	BuildCreateNetdev(model Model, spec NetdevSpec) (WritePlan, error)
	// BuildRemoveNetdev is the mirror image: it deletes a .netdev unit
	// tui-network itself wrote and strips the member lines that referenced it,
	// again as one plan. A unit the tool does not own is refused.
	BuildRemoveNetdev(model Model, name string) (WritePlan, error)

	// Egress resolves how the kernel would reach a gateway, with a read-only
	// `ip route get`. It is the optional reachability probe the Gateways
	// screen runs: a route lookup, never a packet.
	Egress(ctx context.Context, gw Gateway) (Egress, error)
	// BuildSetDefaultGateway builds the live command that makes a gateway the
	// default route at the given metric — the runtime switch and the manual
	// failover both go through it. It is destructive: it changes routing and
	// can drop the session it runs over.
	BuildSetDefaultGateway(gw Gateway, metric int) (Command, error)
	// BuildPersistGateway renders a systemd-networkd drop-in that sets a
	// gateway's default-route priority durably, and returns the diff plus the
	// commands that install and reload it. It is offered only for a
	// networkd-managed interface that already has a .network file to attach to.
	BuildPersistGateway(gw Gateway, metric int) (WritePlan, error)
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
	// Files are every file the plan touches, in the order the commands apply
	// them. A single-file edit leaves it empty and carries Path and Content
	// alone; a VLAN or a bridge fills it, and Diff is then the diff of all of
	// them, one after another, so the dialog shows the whole change at once.
	Files []FileChange
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
