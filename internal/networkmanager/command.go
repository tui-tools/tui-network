package networkmanager

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tui-tools/tui-network/internal/network"
	"github.com/tui-tools/tui-network/internal/networkd"
	"github.com/tui-tools/tui-network/internal/textdiff"
)

// Name is the backend's name, which is also the manifest's backend name the
// version probe is keyed on. The manifest takes lower-case names only.
const Name = "networkmanager"

// Display is how NetworkManager is named to a person: in reasons, dialogs,
// the header and the journal unit.
const Display = "NetworkManager"

// The IPv4 methods the guided editor offers. They are NetworkManager's own
// words for the three things an operator asks of a link's IPv4: take it from
// DHCP, set it by hand, or have none.
const (
	MethodAuto     = "auto"
	MethodManual   = "manual"
	MethodDisabled = "disabled"
)

// capabilities describes what the NetworkManager backend supports. It is
// shared by the real and the fake backend, so --demo behaves like a real run.
var capabilities = network.Capabilities{
	DHCPModes:           []string{MethodAuto, MethodManual, MethodDisabled},
	SupportsUpDown:      true,
	SupportsRenew:       true,
	SupportsFlushCaches: true,
	SupportsRuntimeDNS:  true,
	SupportsFileEdit:    true,
	// VLANs and bridges are profiles of their own on NetworkManager, with
	// ports re-parented through their own profiles; that is a feature of
	// its own and not offered yet.
	SupportsNetdev: false,
	Manager:        Display,
	ManagerLabel:   Display,
	ConfigNoun:     "connection profile",
	Journal:        Display,
}

// Capabilities reports what the NetworkManager backend supports.
func Capabilities() network.Capabilities { return capabilities }

// linkNameRe is the set of characters an interface name may contain. Every
// builder checks the device name against it, because it is the one argument
// that comes from the machine and ends up in an argv run as root.
var linkNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,14}$`)

// checkLink rejects a name that is not a plausible interface name.
func checkLink(link string) error {
	if !linkNameRe.MatchString(link) {
		return fmt.Errorf("networkmanager: %q is not a valid interface name", link)
	}
	return nil
}

// uuidRe is a connection profile's UUID, the only way a profile is named in
// an argv here: a profile's name is free text and can look like an option.
var uuidRe = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

// checkUUID rejects anything that is not a profile UUID.
func checkUUID(uuid string) error {
	if !uuidRe.MatchString(uuid) {
		return fmt.Errorf("networkmanager: %q is not a connection UUID", uuid)
	}
	return nil
}

// BuildLinkAction turns one of the link verbs into an nmcli command. uuid is
// the device's active profile, which renew re-activates; the other verbs act
// on the device alone.
func BuildLinkAction(action, link, uuid string) (network.Command, error) {
	if err := checkLink(link); err != nil {
		return network.Command{}, err
	}
	switch action {
	case network.ActionUp:
		return network.Command{
			Argv:        []string{"nmcli", "device", "connect", link},
			Description: "Connect " + link + " with its best available profile",
		}, nil
	case network.ActionDown:
		return network.Command{
			Argv:        []string{"nmcli", "device", "disconnect", link},
			Description: "Disconnect " + link,
			Destructive: true,
		}, nil
	case network.ActionReconfigure:
		return network.Command{
			Argv: []string{"nmcli", "device", "reapply", link},
			Description: "Reapply the settings of " + link +
				"'s profile to the device",
			Destructive: true,
		}, nil
	case network.ActionRenew:
		// NetworkManager has no lease-renewal verb; re-activating the
		// profile is what starts a fresh DHCP exchange.
		if err := checkUUID(uuid); err != nil {
			return network.Command{}, fmt.Errorf(
				"networkmanager: %s has no active profile to re-activate", link)
		}
		return network.Command{
			Argv: []string{"nmcli", "connection", "up", "uuid", uuid, "ifname", link},
			Description: "Re-activate the profile of " + link +
				", which requests a new lease",
			Destructive: true,
		}, nil
	default:
		return network.Command{}, fmt.Errorf("networkmanager: unknown link action %q", action)
	}
}

// BuildReload makes NetworkManager re-read the profiles on disk. Like
// `networkctl reload` it applies nothing to an active device.
func BuildReload() (network.Command, error) {
	return network.Command{
		Argv:        []string{"nmcli", "connection", "reload"},
		Description: "Reload the NetworkManager connection profiles from disk",
	}, nil
}

// BuildFlushCaches empties systemd-resolved's cache.
func BuildFlushCaches() (network.Command, error) {
	return network.Command{
		Argv:        []string{"resolvectl", "flush-caches"},
		Description: "Flush the resolver cache",
	}, nil
}

// splitFamilies sorts addresses into IPv4 and IPv6, refusing anything that is
// not a plain address. nmcli takes them as two separate settings.
func splitFamilies(addresses []string, what string) ([]string, []string, error) {
	var v4, v6 []string
	for _, value := range addresses {
		addr, err := netip.ParseAddr(value)
		if err != nil {
			return nil, nil, fmt.Errorf("networkmanager: %q is not %s", value, what)
		}
		if addr.Is4() {
			v4 = append(v4, addr.String())
		} else {
			v6 = append(v6, addr.String())
		}
	}
	return v4, v6, nil
}

// domainRe accepts a search domain, including the routing-only "~corp" form
// NetworkManager hands on to systemd-resolved.
var domainRe = regexp.MustCompile(`^~?(\.|[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*)$`)

// checkDomains refuses anything that is not a search domain.
func checkDomains(domains []string) error {
	for _, domain := range domains {
		if !domainRe.MatchString(domain) {
			return fmt.Errorf("networkmanager: %q is not a search domain", domain)
		}
	}
	return nil
}

// BuildSetDNS sets a device's DNS servers at runtime. `nmcli device modify`
// changes the settings applied to the device and not the profile, so, like
// `resolvectl dns`, the change lasts until the profile is activated again.
func BuildSetDNS(link string, servers []string) (network.Command, error) {
	if err := checkLink(link); err != nil {
		return network.Command{}, err
	}
	v4, v6, err := splitFamilies(servers, "a DNS server address")
	if err != nil {
		return network.Command{}, err
	}
	// The servers given replace the ones DHCP hands out, the way
	// `resolvectl dns` replaces them: without ignore-auto-dns NetworkManager
	// would append the lease's servers after them. An empty list goes back to
	// what DHCP provides.
	ignore := "yes"
	if len(servers) == 0 {
		ignore = "no"
	}
	argv := []string{"nmcli", "device", "modify", link,
		"ipv4.dns", strings.Join(v4, ","), "ipv6.dns", strings.Join(v6, ","),
		"ipv4.ignore-auto-dns", ignore, "ipv6.ignore-auto-dns", ignore}
	description := fmt.Sprintf("Set the DNS servers of %s to %s until its "+
		"profile is activated again", link, strings.Join(servers, ", "))
	if len(servers) == 0 {
		description = "Clear the DNS servers set on " + link +
			", so it uses what DHCP provides, until its profile is activated again"
	}
	return network.Command{Argv: argv, Description: description, Destructive: true}, nil
}

// BuildSetDomains sets a device's search domains at runtime, the same way.
func BuildSetDomains(link string, domains []string) (network.Command, error) {
	if err := checkLink(link); err != nil {
		return network.Command{}, err
	}
	if err := checkDomains(domains); err != nil {
		return network.Command{}, err
	}
	argv := []string{"nmcli", "device", "modify", link,
		"ipv4.dns-search", strings.Join(domains, ",")}
	description := fmt.Sprintf("Set the search domains of %s to %s until its "+
		"profile is activated again", link, strings.Join(domains, ", "))
	if len(domains) == 0 {
		description = "Clear the search domains of " + link +
			" until its profile is activated again"
	}
	return network.Command{Argv: argv, Description: description, Destructive: true}, nil
}

// SpecSettings validates a FileSpec and turns it into the profile settings it
// stands for. Only IPv4 is addressed; DNS servers of either family go to the
// family's own list.
func SpecSettings(spec network.FileSpec) ([]network.Setting, error) {
	method := spec.DHCP
	switch method {
	case MethodAuto, MethodManual, MethodDisabled:
	default:
		return nil, fmt.Errorf("networkmanager: the IPv4 method must be one of %s",
			strings.Join(capabilities.DHCPModes, ", "))
	}
	address := strings.TrimSpace(spec.Address)
	if address != "" {
		prefix, err := netip.ParsePrefix(address)
		if err != nil || !prefix.Addr().Is4() {
			return nil, fmt.Errorf(
				"networkmanager: the address must be IPv4 in CIDR form, like 192.0.2.10/24")
		}
	}
	gateway := strings.TrimSpace(spec.Gateway)
	if gateway != "" {
		parsed, err := netip.ParseAddr(gateway)
		if err != nil || !parsed.Is4() {
			return nil, fmt.Errorf("networkmanager: %q is not an IPv4 gateway address", gateway)
		}
	}
	switch method {
	case MethodManual:
		if address == "" {
			return nil, fmt.Errorf(
				"networkmanager: with the manual method the link needs an address")
		}
	case MethodDisabled:
		if address != "" || gateway != "" {
			return nil, fmt.Errorf(
				"networkmanager: with IPv4 disabled the link takes no address or gateway")
		}
	}
	if gateway != "" && address == "" {
		return nil, fmt.Errorf(
			"networkmanager: a static gateway needs a static address beside it")
	}
	v4, v6, err := splitFamilies(spec.DNS, "a DNS server address")
	if err != nil {
		return nil, err
	}
	if err := checkDomains(spec.Domains); err != nil {
		return nil, err
	}
	settings := []network.Setting{
		{Section: "ipv4", Key: "method", Value: method},
		{Section: "ipv4", Key: "addresses", Value: address},
		{Section: "ipv4", Key: "gateway", Value: gateway},
		{Section: "ipv4", Key: "dns", Value: strings.Join(v4, ",")},
		{Section: "ipv4", Key: "dns-search", Value: strings.Join(spec.Domains, ",")},
	}
	if len(v6) > 0 {
		settings = append(settings,
			network.Setting{Section: "ipv6", Key: "dns", Value: strings.Join(v6, ",")})
	}
	return settings, nil
}

// settingArgs renders settings as nmcli's "setting.key value" argument pairs.
func settingArgs(settings []network.Setting) []string {
	args := make([]string, 0, 2*len(settings))
	for _, s := range settings {
		args = append(args, s.Section+"."+s.Key, s.Value)
	}
	return args
}

// BuildModifyProfile builds the commands that write the editor's settings into
// an existing profile and activate it on the device: the profile is changed
// first and applied second, the way `nmcli connection modify` is meant to be
// used, and both are previewed.
func BuildModifyProfile(uuid, link string, settings []network.Setting) ([]network.Command, error) {
	if err := checkUUID(uuid); err != nil {
		return nil, err
	}
	if err := checkLink(link); err != nil {
		return nil, err
	}
	modify := network.Command{
		Argv: append([]string{"nmcli", "connection", "modify", "uuid", uuid},
			settingArgs(settings)...),
		Description: "Write the new IPv4 settings into the profile of " + link,
		Destructive: true,
	}
	up := network.Command{
		Argv:        []string{"nmcli", "connection", "up", "uuid", uuid, "ifname", link},
		Description: "Activate the changed profile on " + link,
		Destructive: true,
	}
	return []network.Command{modify, up}, nil
}

// BuildAddProfile builds the commands that create a wired profile for a device
// that has none and activate it. The profile is named after the device.
func BuildAddProfile(link string, settings []network.Setting) ([]network.Command, error) {
	if err := checkLink(link); err != nil {
		return nil, err
	}
	argv := []string{"nmcli", "connection", "add", "type", "ethernet",
		"con-name", link, "ifname", link}
	add := network.Command{
		Argv:        append(argv, settingArgs(settings)...),
		Description: "Create a wired profile for " + link,
		Destructive: true,
	}
	up := network.Command{
		Argv:        []string{"nmcli", "device", "connect", link},
		Description: "Connect " + link + " with the new profile",
		Destructive: true,
	}
	return []network.Command{add, up}, nil
}

// SettingsDiff is the unified diff the confirm dialog shows for a profile
// change: the settings it touches before and after, in nmcli's own
// "setting.key: value" form. A setting the profile does not carry today reads
// as empty, the way nmcli prints it.
func SettingsDiff(path string, current []network.Setting, change []network.Setting) string {
	before := make([]network.Setting, 0, len(change))
	for _, s := range change {
		value := ""
		for _, c := range current {
			if c.Section == s.Section && c.Key == s.Key {
				value = c.Value
			}
		}
		before = append(before, network.Setting{Section: s.Section, Key: s.Key, Value: value})
	}
	return textdiff.Unified(path, RenderSettings(before), RenderSettings(change))
}

// routeMetricKey is the profile setting that holds a family's route metric.
func routeMetricKey(family string) string {
	if family == "ipv6" {
		return "ipv6.route-metric"
	}
	return "ipv4.route-metric"
}

// maxMetric bounds the route metric a command may carry: the kernel's route
// priority is a 32-bit unsigned field, and NetworkManager stores it as one.
const maxMetric = 0xFFFFFFFF

// checkMetric rejects a metric outside the kernel's route-priority range.
func checkMetric(metric int) error {
	if metric < 0 || metric > maxMetric {
		return fmt.Errorf("networkmanager: %d is not a route metric", metric)
	}
	return nil
}

// BuildSetDefaultGateway makes gw the default route. On a device NetworkManager
// manages it changes the device's applied route metric, so NetworkManager
// itself re-installs the default route at the new priority and does not undo
// it on its next change; on a device it does not manage, the kernel route is
// replaced the way the networkd backend replaces it.
func BuildSetDefaultGateway(gw network.Gateway, metric int) (network.Command, error) {
	if !gw.Managed {
		return networkd.BuildSetDefaultGateway(gw, metric)
	}
	if err := checkLink(gw.Interface); err != nil {
		return network.Command{}, err
	}
	if err := checkMetric(metric); err != nil {
		return network.Command{}, err
	}
	return network.Command{
		Argv: []string{"nmcli", "device", "modify", gw.Interface,
			routeMetricKey(gw.Family), strconv.Itoa(metric)},
		Description: fmt.Sprintf("Make %s the default route via %s (metric %d) "+
			"until its profile is activated again", gw.Interface, gw.Address, metric),
		Destructive: true,
	}, nil
}

// BuildPersistGateway writes the uplink's priority into its profile and
// reapplies the profile to the device.
func BuildPersistGateway(gw network.Gateway, metric int) ([]network.Command, error) {
	if !gw.Managed {
		return nil, fmt.Errorf(
			"networkmanager: %s is not managed by NetworkManager, "+
				"so its priority cannot be made persistent here", gw.Interface)
	}
	if gw.ConfigID == "" {
		return nil, fmt.Errorf(
			"networkmanager: %s has no active profile to keep the priority in", gw.Interface)
	}
	if err := checkUUID(gw.ConfigID); err != nil {
		return nil, err
	}
	if err := checkLink(gw.Interface); err != nil {
		return nil, err
	}
	if err := checkMetric(metric); err != nil {
		return nil, err
	}
	modify := network.Command{
		Argv: []string{"nmcli", "connection", "modify", "uuid", gw.ConfigID,
			routeMetricKey(gw.Family), strconv.Itoa(metric)},
		Description: fmt.Sprintf("Keep metric %d for %s in its profile", metric, gw.Interface),
		Destructive: true,
	}
	reapply := network.Command{
		Argv:        []string{"nmcli", "device", "reapply", gw.Interface},
		Description: "Reapply the profile of " + gw.Interface,
		Destructive: true,
	}
	return []network.Command{modify, reapply}, nil
}

// The D-Bus coordinates of NetworkManager's checkpoint API.
const (
	busName      = "org.freedesktop.NetworkManager"
	busPath      = "/org/freedesktop/NetworkManager"
	busInterface = "org.freedesktop.NetworkManager"
)

// checkpointFlags are the CheckpointCreate flags every guard uses:
// DELETE_NEW_CONNECTIONS (0x2) and DISCONNECT_NEW_DEVICES (0x4), so a rollback
// also removes a profile or a device the change created, not only the ones it
// modified.
const checkpointFlags = 6

// checkpointRe is the object path CheckpointCreate answers with.
var checkpointRe = regexp.MustCompile(`/org/freedesktop/NetworkManager/Checkpoint/[0-9]+`)

// BuildArm returns the busctl call that takes a checkpoint of every device,
// which NetworkManager rolls back by itself after timeout unless it is
// destroyed first.
func BuildArm(timeout time.Duration) (network.Command, error) {
	seconds := int(timeout / time.Second)
	if seconds <= 0 {
		return network.Command{}, fmt.Errorf("networkmanager: a rollback timeout must be positive")
	}
	return network.Command{
		Argv: []string{"busctl", "call", busName, busPath, busInterface,
			"CheckpointCreate", "aouu", "0", strconv.Itoa(seconds),
			strconv.Itoa(checkpointFlags)},
		Description: fmt.Sprintf("Take a NetworkManager checkpoint that rolls "+
			"back on its own in %d s", seconds),
	}, nil
}

// ArmedID reads the checkpoint path from CheckpointCreate's answer
// (`o "/org/freedesktop/NetworkManager/Checkpoint/3"`).
func ArmedID(output string) (string, error) {
	id := checkpointRe.FindString(output)
	if id == "" {
		return "", fmt.Errorf("networkmanager: no checkpoint in %q", strings.TrimSpace(output))
	}
	return id, nil
}

// checkCheckpoint rejects anything that is not a checkpoint object path.
func checkCheckpoint(id string) error {
	if checkpointRe.FindString(id) != id || id == "" {
		return fmt.Errorf("networkmanager: %q is not a checkpoint", id)
	}
	return nil
}

// BuildKeep destroys the checkpoint, which keeps the change.
func BuildKeep(id string) (network.Command, error) {
	if err := checkCheckpoint(id); err != nil {
		return network.Command{}, err
	}
	return network.Command{
		Argv: []string{"busctl", "call", busName, busPath, busInterface,
			"CheckpointDestroy", "o", id},
		Description: "Keep the change: discard the checkpoint",
	}, nil
}

// BuildRollback restores the checkpoint now.
func BuildRollback(id string) (network.Command, error) {
	if err := checkCheckpoint(id); err != nil {
		return network.Command{}, err
	}
	return network.Command{
		Argv: []string{"busctl", "call", busName, busPath, busInterface,
			"CheckpointRollback", "o", id},
		Description: "Roll the change back to the checkpoint",
		Destructive: true,
	}, nil
}
