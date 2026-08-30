package networkd

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/tui-tools/tui-network/internal/network"
)

// The address families systemd reports as numbers, straight from <sys/socket.h>.
const (
	familyIPv4 = 2
	familyIPv6 = 10
)

// rawBytes is how systemd writes an address in JSON: an array of the address
// bytes, not a string. Decoding it into a plain []byte would fail, because
// encoding/json expects base64 there, so the array form is read explicitly.
type rawBytes []byte

// UnmarshalJSON reads the array form, and tolerates a null (a field systemd
// includes but has no value for) as an empty address.
func (r *rawBytes) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*r = nil
		return nil
	}
	var numbers []uint8
	if err := json.Unmarshal(data, &numbers); err != nil {
		return err
	}
	*r = numbers
	return nil
}

// jsonAddress is one entry of a link's Addresses in `networkctl --json`.
// Addresses arrive as arrays of bytes rather than as text, so every one of
// them has to be rebuilt here.
type jsonAddress struct {
	Family         int      `json:"Family"`
	Address        rawBytes `json:"Address"`
	PrefixLength   int      `json:"PrefixLength"`
	ConfigSource   string   `json:"ConfigSource"`
	ConfigProvider rawBytes `json:"ConfigProvider"`
	ScopeString    string   `json:"ScopeString"`
}

// jsonRoute is one entry of a link's Routes in `networkctl --json`.
type jsonRoute struct {
	Family                  int      `json:"Family"`
	Destination             rawBytes `json:"Destination"`
	DestinationPrefixLength int      `json:"DestinationPrefixLength"`
	Gateway                 rawBytes `json:"Gateway"`
	PreferredSource         rawBytes `json:"PreferredSource"`
	Priority                int      `json:"Priority"`
	ProtocolString          string   `json:"ProtocolString"`
	ScopeString             string   `json:"ScopeString"`
	TableString             string   `json:"TableString"`
	TypeString              string   `json:"TypeString"`
	ConfigSource            string   `json:"ConfigSource"`
	ConfigProvider          rawBytes `json:"ConfigProvider"`
}

// jsonServer is a DNS server entry.
type jsonServer struct {
	Family         int      `json:"Family"`
	Address        rawBytes `json:"Address"`
	ConfigSource   string   `json:"ConfigSource"`
	ConfigProvider rawBytes `json:"ConfigProvider"`
}

// jsonDomain is a search domain entry.
type jsonDomain struct {
	Domain       string `json:"Domain"`
	ConfigSource string `json:"ConfigSource"`
}

// jsonLease is the DHCPv4 lease clock networkd exposes. The timestamps are
// microseconds on the CLOCK_BOOTTIME scale, so they are rendered as an age
// rather than as a wall clock date, which they are not.
type jsonLease struct {
	LeaseTimestampUSec uint64 `json:"LeaseTimestampUSec"`
	Timeout1USec       uint64 `json:"Timeout1USec"`
	Timeout2USec       uint64 `json:"Timeout2USec"`
}

// jsonLink is one interface as `networkctl --json` describes it. Only the
// fields tui-network shows are decoded; systemd adds more with every release
// and an unknown one must not break the read.
type jsonLink struct {
	Index               int      `json:"Index"`
	Name                string   `json:"Name"`
	Type                string   `json:"Type"`
	Kind                string   `json:"Kind"`
	Driver              string   `json:"Driver"`
	MTU                 int      `json:"MTU"`
	HardwareAddress     rawBytes `json:"HardwareAddress"`
	AdministrativeState string   `json:"AdministrativeState"`
	OperationalState    string   `json:"OperationalState"`
	CarrierState        string   `json:"CarrierState"`
	OnlineState         string   `json:"OnlineState"`
	NetworkFile         string   `json:"NetworkFile"`
	NetworkFileDropins  []string `json:"NetworkFileDropins"`

	Addresses     []jsonAddress `json:"Addresses"`
	Routes        []jsonRoute   `json:"Routes"`
	DNS           []jsonServer  `json:"DNS"`
	SearchDomains []jsonDomain  `json:"SearchDomains"`

	DHCPv4Client *struct {
		Lease *jsonLease `json:"Lease"`
	} `json:"DHCPv4Client"`
}

// jsonList is the envelope of `networkctl --json=short list`.
type jsonList struct {
	Interfaces []jsonLink `json:"Interfaces"`
}

// ParseListJSON reads `networkctl --json=short list` into links. It is the
// preferred read path: everything the overview shows is in there, already
// typed, with no column widths to guess at.
func ParseListJSON(out string) ([]network.Link, error) {
	var list jsonList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("networkctl list: %w", err)
	}
	links := make([]network.Link, 0, len(list.Interfaces))
	for _, item := range list.Interfaces {
		links = append(links, item.toLink())
	}
	return links, nil
}

// ParseStatusJSON reads `networkctl status --json=short <link>` into one link.
func ParseStatusJSON(out string) (network.Link, error) {
	var item jsonLink
	if err := json.Unmarshal([]byte(out), &item); err != nil {
		return network.Link{}, fmt.Errorf("networkctl status: %w", err)
	}
	return item.toLink(), nil
}

// toLink converts one decoded interface into the backend-neutral model.
func (j jsonLink) toLink() network.Link {
	link := network.Link{
		Index:              j.Index,
		Name:               j.Name,
		Type:               j.Type,
		Kind:               j.Kind,
		Driver:             j.Driver,
		MTU:                j.MTU,
		Setup:              j.AdministrativeState,
		Operational:        j.OperationalState,
		Carrier:            j.CarrierState,
		Online:             j.OnlineState,
		MAC:                formatMAC(j.HardwareAddress),
		NetworkFile:        j.NetworkFile,
		NetworkFileDropins: j.NetworkFileDropins,
	}

	for _, a := range j.Addresses {
		address, ok := formatIP(a.Address)
		if !ok {
			continue
		}
		link.Addresses = append(link.Addresses, network.Address{
			Address:  address,
			Prefix:   a.PrefixLength,
			Family:   familyName(a.Family),
			Source:   a.ConfigSource,
			Scope:    scopeName(a.ScopeString),
			Provider: firstIP(a.ConfigProvider),
		})
		if isDHCP(a.ConfigSource) {
			link.DHCP.Enabled = true
			link.DHCP.Address = address
			link.DHCP.Server = firstIP(a.ConfigProvider)
		}
	}

	// A link's gateways are the gateways of its own default routes, which is
	// where networkctl's own "Gateway:" line comes from too.
	for _, r := range j.Routes {
		if r.DestinationPrefixLength != 0 || len(r.Gateway) == 0 {
			continue
		}
		if gateway, ok := formatIP(r.Gateway); ok {
			link.Gateways = appendUnique(link.Gateways, gateway)
		}
	}

	for _, s := range j.DNS {
		if server, ok := formatIP(s.Address); ok {
			link.DNS = appendUnique(link.DNS, server)
		}
	}
	for _, d := range j.SearchDomains {
		link.SearchDomains = appendUnique(link.SearchDomains, d.Domain)
	}

	if j.DHCPv4Client != nil && j.DHCPv4Client.Lease != nil {
		lease := j.DHCPv4Client.Lease
		link.DHCP.Enabled = true
		link.DHCP.LeaseTimestamp = formatUptime(lease.LeaseTimestampUSec)
		link.DHCP.Timeout1 = formatUptime(lease.Timeout1USec)
		link.DHCP.Timeout2 = formatUptime(lease.Timeout2USec)
	}

	link.Managed = link.Setup != "" && link.Setup != network.SetupUnmanaged
	if !link.Managed {
		link.ReadOnlyReason = unmanagedReason
	}
	return link
}

// unmanagedReason is what the UI says about a link networkd does not own. It
// is deliberately about *who owns the link*, not about what tui-network
// refuses: the tool declines because changing such a link would be undone, or
// would fight, with whoever does own it.
const unmanagedReason = "systemd-networkd reports this link as unmanaged, " +
	"so its configuration belongs to something else"

// ParseRoutesJSON reads `ip -j route` into the routing table. iproute2 prints
// text values here, which is why this parser is so much shorter than the
// networkctl one.
func ParseRoutesJSON(out string) ([]network.Route, error) {
	var raw []struct {
		Dst      string `json:"dst"`
		Gateway  string `json:"gateway"`
		Dev      string `json:"dev"`
		Protocol string `json:"protocol"`
		Scope    string `json:"scope"`
		PrefSrc  string `json:"prefsrc"`
		Metric   int    `json:"metric"`
		Table    string `json:"table"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("ip route: %w", err)
	}
	routes := make([]network.Route, 0, len(raw))
	for _, r := range raw {
		routes = append(routes, network.Route{
			Destination: r.Dst,
			Gateway:     r.Gateway,
			Link:        r.Dev,
			Source:      r.PrefSrc,
			Protocol:    r.Protocol,
			Scope:       r.Scope,
			Metric:      r.Metric,
			Table:       r.Table,
			Family:      routeFamily(r.Dst, r.Gateway),
		})
	}
	return routes, nil
}

// routeFamily decides which family a route belongs to from the addresses it
// carries: "default" alone says nothing, so the gateway settles it.
func routeFamily(destination, gateway string) string {
	for _, candidate := range []string{destination, gateway} {
		host, _, found := strings.Cut(candidate, "/")
		if !found {
			host = candidate
		}
		if addr, err := netip.ParseAddr(host); err == nil {
			if addr.Is4() {
				return "ipv4"
			}
			return "ipv6"
		}
	}
	return ""
}

// ParseListText reads the columns of a plain `networkctl list`. It is the
// fallback for a systemd older than 249, which has no --json, and for a
// machine where networkd is not running at all — there networkctl still lists
// the kernel's links and marks every one of them unmanaged, which is exactly
// what the user needs to see.
func ParseListText(out string) []network.Link {
	var links []network.Link
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "IDX ") ||
			strings.Contains(line, "links listed") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		index, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		link := network.Link{
			Index:       index,
			Name:        fields[1],
			Type:        fields[2],
			Operational: normalizeDash(fields[3]),
			Setup:       normalizeDash(fields[4]),
		}
		link.Managed = link.Setup != "" && link.Setup != network.SetupUnmanaged
		if !link.Managed {
			link.ReadOnlyReason = unmanagedReason
		}
		links = append(links, link)
	}
	return links
}

// normalizeDash turns networkctl's placeholder for "nothing to report" into an
// empty string, so the UI decides how to render an absence.
func normalizeDash(s string) string {
	if s == "-" || s == "n/a" {
		return ""
	}
	return s
}

// ParseStatusText reads the `Key: value` block of a plain `networkctl status
// <link>`, the fallback path when JSON is unavailable. Continuation lines are
// indented with no key, and belong to the previous key.
func ParseStatusText(out string) network.Link {
	var link network.Link
	var key string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// The first line is "● 4: veth0" — the index and the name.
		if trimmed := strings.TrimLeft(line, "●* \t"); strings.Contains(trimmed, ": ") &&
			!strings.HasPrefix(line, " ") && link.Name == "" {
			head, name, _ := strings.Cut(trimmed, ": ")
			if index, err := strconv.Atoi(strings.TrimSpace(head)); err == nil {
				link.Index, link.Name = index, strings.TrimSpace(name)
				continue
			}
		}
		field, value, found := strings.Cut(line, ": ")
		if found {
			key = strings.TrimSpace(field)
		}
		value = strings.TrimSpace(value)
		if !found {
			// A continuation line: the value belongs to the previous key.
			value = strings.TrimSpace(line)
		}
		if value == "" {
			continue
		}
		applyStatusField(&link, key, value)
	}
	link.Managed = link.Setup != "" && link.Setup != network.SetupUnmanaged
	if !link.Managed {
		link.ReadOnlyReason = unmanagedReason
	}
	return link
}

// applyStatusField folds one `networkctl status` line into the link.
func applyStatusField(link *network.Link, key, value string) {
	switch key {
	case "Network File":
		link.NetworkFile = normalizeDash(value)
	case "State":
		// "routable (configured)" is the operational state and the setup
		// state on one line.
		operational, setup, found := strings.Cut(value, " (")
		link.Operational = strings.TrimSpace(operational)
		if found {
			link.Setup = strings.TrimSpace(strings.TrimSuffix(setup, ")"))
		}
	case "Online state":
		link.Online = value
	case "Type":
		link.Type = value
	case "Kind":
		link.Kind = value
	case "Driver":
		link.Driver = value
	case "Hardware Address":
		link.MAC = strings.Fields(value)[0]
	case "MTU":
		if mtu, err := strconv.Atoi(strings.Fields(value)[0]); err == nil {
			link.MTU = mtu
		}
	case "Address":
		link.Addresses = append(link.Addresses, parseTextAddress(value))
		if strings.Contains(value, "DHCP4") || strings.Contains(value, "DHCP6") {
			link.DHCP.Enabled = true
			link.DHCP.Address = strings.Fields(value)[0]
			if _, server, found := strings.Cut(value, "via "); found {
				link.DHCP.Server = strings.TrimSuffix(strings.TrimSpace(server), ")")
			}
		}
	case "Gateway":
		link.Gateways = appendUnique(link.Gateways, strings.Fields(value)[0])
	case "DNS":
		for _, server := range strings.Fields(value) {
			link.DNS = appendUnique(link.DNS, server)
		}
	case "Search Domains":
		for _, domain := range strings.Fields(value) {
			link.SearchDomains = appendUnique(link.SearchDomains, domain)
		}
	case "DHCP4 Client ID":
		link.DHCP.ClientID, link.DHCP.Enabled = value, true
	case "DHCP6 Client DUID":
		link.DHCP.DUID = value
	}
}

// parseTextAddress reads "192.0.2.98 (DHCP4 via 192.0.2.1)" into an Address.
// The text form carries no prefix length, which is one reason the JSON path is
// preferred wherever it exists.
func parseTextAddress(value string) network.Address {
	fields := strings.Fields(value)
	address := network.Address{Address: fields[0], Scope: "global"}
	if host, prefix, found := strings.Cut(fields[0], "/"); found {
		address.Address = host
		if n, err := strconv.Atoi(prefix); err == nil {
			address.Prefix = n
		}
	}
	if addr, err := netip.ParseAddr(address.Address); err == nil {
		if addr.Is4() {
			address.Family = "ipv4"
		} else {
			address.Family = "ipv6"
			if addr.IsLinkLocalUnicast() {
				address.Scope = "link"
			}
		}
	}
	switch {
	case strings.Contains(value, "DHCP4"):
		address.Source = "DHCPv4"
	case strings.Contains(value, "DHCP6"):
		address.Source = "DHCPv6"
	}
	return address
}

// ParseResolvectlDNS reads `resolvectl dns` or `resolvectl domain`, which
// print one "Link N (name): values" line per link, wrapping long lists onto
// indented continuation lines. The global entry has no link.
//
// It returns the per-link values keyed by link name, plus the global ones.
func ParseResolvectlDNS(out string) (perLink map[string][]string, global []string) {
	perLink = map[string][]string{}
	current := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// A continuation line is indented and carries only values.
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) &&
			!strings.HasPrefix(strings.TrimSpace(line), "Link ") &&
			!strings.HasPrefix(strings.TrimSpace(line), "Global") {
			values := strings.Fields(line)
			if current == "" {
				global = append(global, values...)
			} else {
				perLink[current] = append(perLink[current], values...)
			}
			continue
		}
		head, values, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		head = strings.TrimSpace(head)
		if head == "Global" {
			current = ""
			global = append(global, strings.Fields(values)...)
			continue
		}
		name, ok := linkNameFromHead(head)
		if !ok {
			continue
		}
		current = name
		if fields := strings.Fields(values); len(fields) > 0 {
			perLink[name] = append(perLink[name], fields...)
		} else if _, seen := perLink[name]; !seen {
			// Record the link even with no servers, so the caller can tell
			// "resolved knows this link and it has none" from "not seen".
			perLink[name] = nil
		}
	}
	return perLink, global
}

// linkNameFromHead pulls "wlan0" out of "Link 3 (wlan0)".
func linkNameFromHead(head string) (string, bool) {
	start := strings.Index(head, "(")
	end := strings.LastIndex(head, ")")
	if !strings.HasPrefix(head, "Link ") || start < 0 || end < start {
		return "", false
	}
	return head[start+1 : end], true
}

// ParseNetworkFile reads a systemd .network file into its sections. It is a
// read-only view: comments and ordering are kept in Raw, and only the
// `Key=Value` lines are decoded, which is all the guided form needs.
func ParseNetworkFile(path, raw string) network.ConfigFile {
	file := network.ConfigFile{Path: path, Raw: raw}
	section := ""
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") ||
			strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = trimmed[1 : len(trimmed)-1]
			continue
		}
		key, value, found := strings.Cut(trimmed, "=")
		if !found {
			continue
		}
		file.Settings = append(file.Settings, network.Setting{
			Section: section,
			Key:     strings.TrimSpace(key),
			Value:   strings.TrimSpace(value),
		})
	}
	if name, ok := file.Get("Match", "Name"); ok {
		file.MatchName = name
	}
	return file
}

// formatMAC renders a hardware address, and returns an empty string for a link
// that has none (loopback, tunnels).
func formatMAC(raw rawBytes) string {
	if len(raw) == 0 {
		return ""
	}
	return net.HardwareAddr(raw).String()
}

// formatIP renders an address systemd gave as raw bytes.
func formatIP(raw rawBytes) (string, bool) {
	addr, ok := netip.AddrFromSlice(raw)
	if !ok {
		return "", false
	}
	return addr.Unmap().String(), true
}

// firstIP renders an optional address, empty when there is none.
func firstIP(raw rawBytes) string {
	if address, ok := formatIP(raw); ok {
		return address
	}
	return ""
}

// familyName names an AF_* constant.
func familyName(family int) string {
	switch family {
	case familyIPv4:
		return "ipv4"
	case familyIPv6:
		return "ipv6"
	default:
		return ""
	}
}

// scopeName defaults an absent scope to "global", which is what a scope of 0
// means in the kernel.
func scopeName(scope string) string {
	if scope == "" {
		return "global"
	}
	return scope
}

// isDHCP reports whether a config source is one of the DHCP clients.
func isDHCP(source string) bool {
	return strings.HasPrefix(source, "DHCP")
}

// formatUptime renders a CLOCK_BOOTTIME microsecond stamp as a duration since
// boot. systemd reports lease times on that clock, so a wall-clock date would
// be a lie; "2h13m since boot" is the honest reading.
func formatUptime(usec uint64) string {
	if usec == 0 {
		return ""
	}
	d := time.Duration(usec) * time.Microsecond
	return d.Truncate(time.Second).String() + " since boot"
}

// appendUnique adds a value once, keeping the order the backend reported.
func appendUnique(list []string, value string) []string {
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
