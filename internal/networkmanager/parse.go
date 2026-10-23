package networkmanager

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/tui-tools/tui-network/internal/network"
)

// Device is one device as `nmcli -t -f GENERAL,IP4,DHCP4,IP6 device show`
// reports it. It is the raw read; buildModel turns it into a network.Link.
type Device struct {
	Name string
	// Type is NetworkManager's device type ("ethernet", "wifi", "bridge").
	Type string
	// Driver is the kernel driver ("veth", "e1000e", "unknown").
	Driver string
	HWAddr string
	MTU    int
	// StateCode and State are GENERAL.STATE split in two: "100" and
	// "connected (externally)".
	StateCode int
	State     string
	// Software reports a virtual device (GENERAL.IS-SOFTWARE).
	Software bool
	// Connection and ConnectionUUID name the active profile, empty when none.
	Connection     string
	ConnectionUUID string

	IP4Addresses []string
	IP6Addresses []string
	IP4Gateway   string
	IP6Gateway   string
	DNS          []string
	Searches     []string
	// DHCP4 holds the DHCPv4 lease options ("dhcp_server_identifier" ->
	// "192.0.2.1"), empty for a device without a lease.
	DHCP4 map[string]string
}

// The device states NetworkManager reports, by their numeric code. They come
// from NMDeviceState and have been stable since NetworkManager 0.9.10.
const (
	stateUnmanaged    = 10
	stateUnavailable  = 20
	stateDisconnected = 30
	stateActivated    = 100
	stateDeactivating = 110
	stateFailed       = 120
)

// externallyMarker is how NetworkManager says it only observes a device
// something else configured: "connected (externally)".
const externallyMarker = "(externally)"

// External reports a device NetworkManager did not configure and only watches.
func (d Device) External() bool { return strings.Contains(d.State, externallyMarker) }

// Profile is one connection profile: the row `nmcli connection show` lists,
// plus the settings `nmcli connection show uuid <uuid>` prints for it.
type Profile struct {
	Name     string
	UUID     string
	Type     string
	Device   string
	Filename string
	Settings []network.Setting
}

// Get returns one setting of the profile ("ipv4", "method").
func (p Profile) Get(section, key string) string {
	for _, s := range p.Settings {
		if s.Section == section && s.Key == key {
			return s.Value
		}
	}
	return ""
}

// splitKey splits "IP4.ADDRESS[2]:192.0.2.10/24" into its key, with any list
// index removed ("IP4.ADDRESS"), and its value. The multi-line terse output
// prints values unescaped, so the first colon is the only separator.
func splitKey(line string) (string, string, bool) {
	key, value, ok := strings.Cut(line, ":")
	if !ok {
		return "", "", false
	}
	if i := strings.IndexByte(key, '['); i >= 0 && strings.HasSuffix(key, "]") {
		key = key[:i]
	}
	return key, value, true
}

// ParseDeviceShow reads `nmcli -t -f GENERAL,IP4,DHCP4,IP6 device show`: one
// block of KEY:value lines per device, blocks separated by an empty line.
func ParseDeviceShow(out string) []Device {
	var devices []Device
	var current *Device
	flush := func() {
		if current != nil && current.Name != "" {
			devices = append(devices, *current)
		}
		current = nil
	}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		key, value, ok := splitKey(line)
		if !ok {
			continue
		}
		if key == "GENERAL.DEVICE" {
			// A new device starts even without the blank line between them.
			flush()
			current = &Device{Name: value}
			continue
		}
		if current == nil {
			continue
		}
		applyDeviceField(current, key, value)
	}
	flush()
	return devices
}

// applyDeviceField folds one KEY:value line into a device.
func applyDeviceField(d *Device, key, value string) {
	switch key {
	case "GENERAL.TYPE":
		d.Type = value
	case "GENERAL.DRIVER":
		d.Driver = value
	case "GENERAL.HWADDR":
		d.HWAddr = strings.ToLower(value)
	case "GENERAL.MTU":
		d.MTU, _ = strconv.Atoi(value)
	case "GENERAL.STATE":
		d.StateCode, d.State = parseState(value)
	case "GENERAL.IS-SOFTWARE":
		d.Software = value == "yes"
	case "GENERAL.CONNECTION":
		d.Connection = value
	case "GENERAL.CON-UUID":
		d.ConnectionUUID = value
	case "IP4.ADDRESS":
		d.IP4Addresses = appendValue(d.IP4Addresses, value)
	case "IP6.ADDRESS":
		d.IP6Addresses = appendValue(d.IP6Addresses, value)
	case "IP4.GATEWAY":
		d.IP4Gateway = value
	case "IP6.GATEWAY":
		d.IP6Gateway = value
	case "IP4.DNS", "IP6.DNS":
		d.DNS = appendValue(d.DNS, value)
	// IP4.DOMAIN became IP4.SEARCHES in NetworkManager 1.44; both are read
	// so the parser serves the releases on either side of the rename.
	case "IP4.DOMAIN", "IP6.DOMAIN", "IP4.SEARCHES", "IP6.SEARCHES":
		d.Searches = appendValue(d.Searches, value)
	case "DHCP4.OPTION":
		name, option, ok := strings.Cut(value, "=")
		if !ok {
			return
		}
		if d.DHCP4 == nil {
			d.DHCP4 = map[string]string{}
		}
		d.DHCP4[strings.TrimSpace(name)] = strings.TrimSpace(option)
	}
}

// appendValue adds a non-empty value once.
func appendValue(list []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return list
	}
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

// parseState splits "100 (connected (externally))" into 100 and
// "connected (externally)". An unparseable state keeps its text.
func parseState(value string) (int, string) {
	number, rest, _ := strings.Cut(strings.TrimSpace(value), " ")
	code, err := strconv.Atoi(number)
	if err != nil {
		return 0, strings.TrimSpace(value)
	}
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "(") && strings.HasSuffix(rest, ")") {
		rest = rest[1 : len(rest)-1]
	}
	return code, rest
}

// SplitTerse splits one line of nmcli's tabular terse output into its
// columns. A colon inside a value is escaped as `\:` and a backslash as `\\`,
// which is what lets a profile called "Office: 2nd floor" survive the trip.
func SplitTerse(line string) []string {
	var fields []string
	var b strings.Builder
	escaped := false
	for _, r := range line {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == ':':
			fields = append(fields, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	return append(fields, b.String())
}

// ParseConnectionList reads `nmcli -t -f NAME,UUID,TYPE,DEVICE,FILENAME
// connection show`.
func ParseConnectionList(out string) []Profile {
	var profiles []Profile
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := SplitTerse(line)
		if len(fields) < 5 {
			continue
		}
		profiles = append(profiles, Profile{
			Name: fields[0], UUID: fields[1], Type: fields[2],
			Device: fields[3], Filename: fields[4],
		})
	}
	return profiles
}

// shownSettings are the profile settings the detail screen shows and the
// editor reads. A profile carries a hundred or more; these are the ones that
// decide how a link is addressed and resolved, which is what this tool edits.
var shownSettings = map[string]bool{
	"connection.id": true, "connection.uuid": true, "connection.type": true,
	"connection.interface-name": true, "connection.autoconnect": true,
	"connection.zone":      true,
	"ipv4.method":          true,
	"ipv4.addresses":       true,
	"ipv4.gateway":         true,
	"ipv4.dns":             true,
	"ipv4.dns-search":      true,
	"ipv4.route-metric":    true,
	"ipv4.routes":          true,
	"ipv4.never-default":   true,
	"ipv4.ignore-auto-dns": true,
	"ipv6.method":          true,
	"ipv6.addresses":       true,
	"ipv6.gateway":         true,
	"ipv6.dns":             true,
	"ipv6.dns-search":      true,
	"ipv6.route-metric":    true,
}

// ParseProfileSettings reads `nmcli -t -f connection,ipv4,ipv6 connection show
// uuid <uuid>`: one setting.key:value line each. Only shownSettings are kept,
// and empty values are dropped, so what the detail screen lists is what the
// profile actually says.
func ParseProfileSettings(out string) []network.Setting {
	var settings []network.Setting
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		key, value, ok := strings.Cut(line, ":")
		if !ok || !shownSettings[key] {
			continue
		}
		if strings.TrimSpace(value) == "" || value == "--" {
			continue
		}
		section, name, _ := strings.Cut(key, ".")
		settings = append(settings, network.Setting{
			Section: section, Key: name, Value: value,
		})
	}
	return settings
}

// ParseVersion reads `nmcli --version` ("nmcli tool, version 1.46.0").
func ParseVersion(out string) string {
	_, rest, ok := strings.Cut(out, "version ")
	if !ok {
		return ""
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// ipLink is the part of `ip -j link` the model takes: the kernel's index,
// which NetworkManager does not print.
type ipLink struct {
	Index int    `json:"ifindex"`
	Name  string `json:"ifname"`
}

// ParseLinkIndexes reads `ip -j link` into a name-to-index map.
func ParseLinkIndexes(out string) (map[string]int, error) {
	var links []ipLink
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		return nil, fmt.Errorf("networkmanager: ip -j link: %w", err)
	}
	indexes := make(map[string]int, len(links))
	for _, link := range links {
		indexes[link.Name] = link.Index
	}
	return indexes, nil
}

// BuildModel turns what nmcli reported into the backend-neutral model: one
// link per device, and one configuration per profile that is bound to a
// device. Routes come from `ip -j route` and are passed in already parsed.
func BuildModel(devices []Device, profiles []Profile, indexes map[string]int) network.Model {
	model := network.Model{Backend: Name, Running: true}
	byUUID := map[string]Profile{}
	for _, profile := range profiles {
		byUUID[profile.UUID] = profile
	}

	for _, device := range devices {
		profile, hasProfile := byUUID[device.ConnectionUUID]
		model.Links = append(model.Links, buildLink(device, profile, hasProfile, indexes))
	}
	sort.SliceStable(model.Links, func(i, j int) bool {
		a, b := model.Links[i], model.Links[j]
		if a.Index != b.Index && a.Index > 0 && b.Index > 0 {
			return a.Index < b.Index
		}
		return a.Name < b.Name
	})

	for _, profile := range profiles {
		if len(profile.Settings) == 0 {
			continue
		}
		model.ConfigFiles = append(model.ConfigFiles, profileConfig(profile))
	}
	return model
}

// ProfilePath is where a profile shows up as a configuration: its file when
// NetworkManager reports one, and its name otherwise (an in-memory profile).
func ProfilePath(profile Profile) string {
	if profile.Filename != "" {
		return profile.Filename
	}
	return "connection " + profile.Name
}

// profileConfig renders a profile as a network.ConfigFile, so the detail
// screen and the gateway view read it the way they read a .network file.
func profileConfig(profile Profile) network.ConfigFile {
	file := network.ConfigFile{
		Path:      ProfilePath(profile),
		Raw:       RenderSettings(profile.Settings),
		Settings:  profile.Settings,
		MatchName: profile.Get("connection", "interface-name"),
		ID:        profile.UUID,
		Name:      profile.Name,
	}
	if profile.Device != "" {
		file.Links = []string{profile.Device}
	}
	return file
}

// RenderSettings prints settings the way `nmcli connection show` does, one
// "setting.key: value" per line. It is what the editor diffs.
func RenderSettings(settings []network.Setting) string {
	var b strings.Builder
	for _, s := range settings {
		fmt.Fprintf(&b, "%s.%s: %s\n", s.Section, s.Key, s.Value)
	}
	return b.String()
}

// buildLink maps one device onto a network.Link.
func buildLink(device Device, profile Profile, hasProfile bool,
	indexes map[string]int) network.Link {
	link := network.Link{
		Index:         indexes[device.Name],
		Name:          device.Name,
		Type:          linkType(device),
		Kind:          linkKind(device),
		Setup:         device.State,
		Operational:   operational(device),
		MAC:           device.HWAddr,
		MTU:           device.MTU,
		Driver:        device.Driver,
		DNS:           device.DNS,
		SearchDomains: device.Searches,
	}
	if link.MAC == "00:00:00:00:00:00" {
		link.MAC = ""
	}
	if link.Driver == "unknown" {
		link.Driver = ""
	}
	for _, gateway := range []string{device.IP4Gateway, device.IP6Gateway} {
		if gateway != "" && gateway != "--" {
			link.Gateways = append(link.Gateways, gateway)
		}
	}
	for _, cidr := range device.IP4Addresses {
		link.Addresses = append(link.Addresses, address(cidr, "ipv4", profile, device))
	}
	for _, cidr := range device.IP6Addresses {
		link.Addresses = append(link.Addresses, address(cidr, "ipv6", profile, device))
	}
	if hasProfile {
		link.NetworkFile = ProfilePath(profile)
	}
	if len(device.DHCP4) > 0 {
		link.DHCP = network.DHCP{
			Enabled:        true,
			Server:         device.DHCP4["dhcp_server_identifier"],
			Address:        device.DHCP4["ip_address"],
			LeaseTimestamp: leaseTime(device.DHCP4),
			Timeout1:       device.DHCP4["renewal_time"],
			Timeout2:       device.DHCP4["rebinding_time"],
		}
	}
	link.Managed, link.ReadOnlyReason = writable(device)
	return link
}

// leaseTime renders the lease length NetworkManager reports, in seconds.
func leaseTime(options map[string]string) string {
	if seconds := options["dhcp_lease_time"]; seconds != "" {
		return seconds + "s lease"
	}
	return ""
}

// writable decides whether tui-network may change a device, and why not.
func writable(device Device) (bool, string) {
	switch {
	case device.Type == "loopback":
		return false, "the loopback link never needs to be changed"
	case device.StateCode == stateUnmanaged || device.StateCode == 0:
		return false, "NetworkManager does not manage this link, " +
			"so tui-network shows it read-only"
	case device.External():
		return false, "this link was configured outside NetworkManager, " +
			"which only observes it, so tui-network shows it read-only"
	default:
		return true, ""
	}
}

// linkType maps NetworkManager's device type onto the kernel link type the
// networkd backend shows, so the TYPE column reads the same on both.
func linkType(device Device) string {
	switch device.Type {
	case "ethernet":
		return "ether"
	case "wifi":
		return "wlan"
	default:
		return device.Type
	}
}

// linkKind names a virtual device's kind, empty for hardware.
func linkKind(device Device) string {
	if !device.Software || device.Type == "loopback" {
		return ""
	}
	if device.Driver == "veth" {
		return "veth"
	}
	if device.Type == "ethernet" {
		return ""
	}
	return device.Type
}

// operational maps a device state onto the operational words the links table
// colours by.
func operational(device Device) string {
	switch {
	case device.StateCode == stateActivated:
		if len(device.IP4Gateway) > 0 || hasGlobal(device) {
			return "routable"
		}
		return "carrier"
	case device.StateCode == stateUnmanaged:
		return "unmanaged"
	case device.StateCode == stateUnavailable:
		return "no-carrier"
	case device.StateCode == stateDisconnected, device.StateCode == stateFailed:
		return "off"
	case device.StateCode > stateDisconnected && device.StateCode < stateActivated,
		device.StateCode == stateDeactivating:
		return "configuring"
	default:
		return device.State
	}
}

// hasGlobal reports whether a device carries any global address.
func hasGlobal(device Device) bool {
	for _, cidr := range append(append([]string{}, device.IP4Addresses...), device.IP6Addresses...) {
		if scopeOf(cidr) == "global" {
			return true
		}
	}
	return false
}

// address builds one network.Address, working out where it came from: an
// address the profile lists is static, the rest came from DHCP or router
// advertisements when the profile asks for them, and from somewhere else
// otherwise.
func address(cidr, family string, profile Profile, device Device) network.Address {
	addr, prefix := cidr, 0
	if a, p, ok := strings.Cut(cidr, "/"); ok {
		addr = a
		prefix, _ = strconv.Atoi(p)
	}
	result := network.Address{
		Address: addr, Prefix: prefix, Family: family, Scope: scopeOf(cidr),
	}
	method := profile.Get(family, "method")
	switch {
	case device.External():
		result.Source = "foreign"
	case containsAddress(profile.Get(family, "addresses"), cidr):
		result.Source = "static"
	case result.Scope == "link" || result.Scope == "host":
		result.Source = "kernel"
	case family == "ipv4" && method == "auto":
		result.Source = "DHCPv4"
		result.Provider = device.DHCP4["dhcp_server_identifier"]
	case family == "ipv6" && (method == "auto" || method == "dhcp"):
		result.Source = "RA"
	default:
		result.Source = "foreign"
	}
	return result
}

// containsAddress reports whether a profile's address list names a CIDR.
// nmcli prints the list comma separated.
func containsAddress(list, cidr string) bool {
	for _, item := range strings.Split(list, ",") {
		if strings.TrimSpace(item) == cidr {
			return true
		}
	}
	return false
}

// scopeOf works out an address's scope from the address itself, which is
// what the kernel does: loopback is host scope, link-local is link scope.
func scopeOf(cidr string) string {
	addr, _, _ := strings.Cut(cidr, "/")
	parsed, err := netip.ParseAddr(addr)
	if err != nil {
		return ""
	}
	switch {
	case parsed.IsLoopback():
		return "host"
	case parsed.IsLinkLocalUnicast():
		return "link"
	default:
		return "global"
	}
}
