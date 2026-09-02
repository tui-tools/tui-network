package dhcpd

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tui-tools/tui-network/internal/dhcp"
	"github.com/tui-tools/tui-network/internal/textdiff"
)

// The third DHCP server this tool reads is not a package at all: it is
// systemd-networkd's own, turned on by DHCPServer=yes in a .network unit and
// configured by that unit's [DHCPServer] section. It is what an Omarchy Router
// runs — the LAN unit that holds the gateway address hands out the leases too
// — so on such a machine the DHCP screen would otherwise have nothing to say.
//
// Everything in this file is pure: parsing the unit and its drop-ins, working
// the pool arithmetic out of PoolOffset=/PoolSize=, and rendering the drop-in
// the mutations write. The host-facing half — finding the units, reading the
// leases through networkctl, staging and installing — lives in backend.go with
// the other two servers.

// NetworkdDropinName is the file the DHCP mutations write, inside the unit's
// own drop-in directory. One owned name means a second change replaces the
// first rather than piling drop-ins up, and it keeps every write out of the
// unit the administrator (or omarchy-router-nics) maintains.
const NetworkdDropinName = "50-tui-network-dhcp.conf"

// networkdConfigDir is the only directory tui-network writes a drop-in to. A
// unit read from /run or /usr/lib still gets its drop-in here, because
// systemd-networkd reads <name>.network.d/ from every one of its search
// directories and /etc wins.
const networkdConfigDir = "/etc/systemd/network"

// networkdConfigDirs are the directories a .network unit can come from, in the
// order systemd-networkd searches them: an earlier one wins the name.
var networkdConfigDirs = []string{
	networkdConfigDir,
	"/run/systemd/network",
	"/usr/lib/systemd/network",
	"/lib/systemd/network",
}

// networkSuffix is the extension of a .network unit.
const networkSuffix = ".network"

// networkdUnitName is the systemd unit `networkctl reload` drives, named here
// only for the description of the apply command.
const networkdUnitName = "systemd-networkd"

// domainOptionCode is DHCP option 15, the domain name. systemd-networkd has no
// dedicated key for it — [DHCPServer] carries EmitDNS=/DNS=, EmitNTP=/NTP= and
// EmitRouter=/Router=, but nothing for the domain, and the EmitDomains=
// setting belongs to [IPv6SendRA], which is router advertisement rather than
// DHCP (man systemd.network, systemd 257). The domain is therefore emitted as
// a raw option, which is what SendOption= exists for.
const domainOptionCode = "15"

// networkdCapabilities is what the networkd DHCP server offers: everything
// dnsmasq offers, through one drop-in rather than three files.
var networkdCapabilities = dhcp.Capabilities{
	SupportsAddReservation:    true,
	SupportsRemoveReservation: true,
	SupportsSetPoolRange:      true,
	SupportsSetOptions:        true,
}

// NetworkdFile is one configuration file of a .network unit — the unit itself
// or one of its drop-ins — in the order systemd-networkd reads it.
type NetworkdFile struct {
	Path string
	Raw  string
}

// NetworkdUnit is one .network unit that runs a DHCP server, with its drop-ins
// already folded in the way networkd folds them: a scalar the last file sets
// wins, a list an empty assignment clears, and every [DHCPServerStaticLease]
// section adds a reservation.
type NetworkdUnit struct {
	// Path is the unit file itself, and Dropins the drop-ins applied over it,
	// in read order.
	Path    string
	Dropins []string
	// Link is the first Name= pattern of the [Match] section — the interface
	// the server serves, which is also the link the lease read asks about.
	Link string
	// Address is the first [Network] or [Address] Address= with a prefix
	// length: the subnet the pool is carved out of. ServerAddress= in
	// [DHCPServer] wins it when set, which is what networkd itself does.
	Address string
	// Enabled reports DHCPServer=yes in [Network].
	Enabled bool
	// HasSection reports that a [DHCPServer] section exists at all. A unit can
	// have one without DHCPServer=yes, which is a server configured and
	// switched off — worth showing rather than hiding.
	HasSection bool

	// PoolOffset and PoolSize are the pool keys as written, zero meaning "use
	// the default", exactly as systemd reads them.
	PoolOffset int
	PoolSize   int
	// DefaultLeaseTimeSec and MaxLeaseTimeSec are the lease clocks as written
	// ("1h", "3600"), empty when the unit leaves systemd's defaults.
	DefaultLeaseTimeSec string
	MaxLeaseTimeSec     string
	// EmitDNS, EmitRouter and EmitNTP are the three emit switches, nil when
	// the unit does not mention them (all three default to yes).
	EmitDNS    *bool
	EmitRouter *bool
	EmitNTP    *bool
	// DNS and NTP are the advertised server lists, which may carry the
	// _server_address token systemd resolves to the server's own address.
	DNS []string
	NTP []string
	// Router is the advertised gateway, empty when the server advertises its
	// own address.
	Router string
	// Domain is the domain name emitted through SendOption=15, empty when the
	// unit sends none.
	Domain string
	// Leases are the [DHCPServerStaticLease] reservations, each carrying the
	// file it was read from so a removal knows whether it may rewrite it.
	Leases []dhcp.Reservation
}

// HasSubnet reports whether the unit's DHCP server has a subnet this screen
// can describe: a concrete IPv4 address with a prefix length.
//
// It is what keeps systemd's own container and VM templates
// (/usr/lib/systemd/network/80-container-ve.network and friends) off the
// screen. Those really do run a DHCP server, but on `Address=0.0.0.0/28` — a
// null address systemd fills in per interface — so there is no pool to show
// and nothing an edit could name.
func (u NetworkdUnit) HasSubnet() bool {
	prefix, err := netip.ParsePrefix(u.Address)
	return err == nil && prefix.Addr().Is4() && !prefix.Addr().IsUnspecified()
}

// iniLineRe splits a `Key=value` line, tolerating the spaces systemd tolerates.
var iniLineRe = regexp.MustCompile(`^\s*([A-Za-z0-9_-]+)\s*=(.*)$`)

// sectionRe matches a `[Section]` header.
var sectionRe = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*$`)

// ParseNetworkdUnit folds a unit and its drop-ins, in read order, into one
// NetworkdUnit. files[0] is the unit; the rest are its drop-ins.
//
// It follows systemd's own merge rules rather than a simpler "last file wins":
// a scalar assigned again is replaced, a list setting (DNS=, NTP=, SendOption=)
// accumulates and an empty assignment clears everything set before it, and a
// [DHCPServerStaticLease] section is additive — which is why a reservation
// declared in the unit itself cannot be taken back by a drop-in, and why the
// removal refuses one.
func ParseNetworkdUnit(files []NetworkdFile) NetworkdUnit {
	var unit NetworkdUnit
	if len(files) > 0 {
		unit.Path = files[0].Path
	}
	for i, file := range files {
		if i > 0 {
			unit.Dropins = append(unit.Dropins, file.Path)
		}
		applyNetworkdFile(&unit, file)
	}
	return unit
}

// applyNetworkdFile folds one file's sections over the unit read so far.
func applyNetworkdFile(unit *NetworkdUnit, file NetworkdFile) {
	section := ""
	// A static lease is only complete at the end of its section, so it is
	// gathered here and flushed when the next section starts or the file ends.
	var lease dhcp.Reservation
	inLease := false
	flush := func() {
		if inLease && lease.MAC != "" && lease.IP != "" {
			lease.Source = file.Path
			lease.Family = "ipv4"
			unit.Leases = append(unit.Leases, lease)
		}
		lease, inLease = dhcp.Reservation{}, false
	}

	for _, line := range strings.Split(file.Raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") ||
			strings.HasPrefix(trimmed, ";") {
			continue
		}
		if match := sectionRe.FindStringSubmatch(trimmed); match != nil {
			flush()
			section = strings.ToLower(match[1])
			inLease = section == "dhcpserverstaticlease"
			continue
		}
		match := iniLineRe.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		key, value := strings.ToLower(match[1]), strings.TrimSpace(match[2])
		switch section {
		case "match":
			if key == "name" && unit.Link == "" {
				unit.Link = firstField(value)
			}
		case "network", "address":
			applyNetworkSection(unit, key, value)
		case "dhcpserver":
			unit.HasSection = true
			applyDHCPServerSection(unit, key, value, file.Path)
		case "dhcpserverstaticlease":
			switch key {
			case "macaddress":
				lease.MAC = strings.ToLower(value)
			case "address":
				lease.IP = value
			}
		}
	}
	flush()
}

// applyNetworkSection reads the two [Network] (or [Address]) keys the DHCP
// screen needs: the switch that turns the server on, and the address whose
// subnet the pool is carved out of.
func applyNetworkSection(unit *NetworkdUnit, key, value string) {
	switch key {
	case "dhcpserver":
		unit.Enabled = parseBool(value, false)
	case "address":
		if unit.Address == "" && isPrefixedAddress(value) {
			unit.Address = value
		}
	}
}

// applyDHCPServerSection reads the [DHCPServer] keys the screen shows and the
// editor writes. Keys this tool does not manage (BootServerAddress=,
// RelayTarget=, the vendor options…) are left alone: they stay in the unit and
// the drop-in never mentions them.
func applyDHCPServerSection(unit *NetworkdUnit, key, value, path string) {
	_ = path // reserved: the [DHCPServer] keys carry no per-file source today
	switch key {
	case "serveraddress":
		if isPrefixedAddress(value) {
			unit.Address = value
		}
	case "pooloffset":
		unit.PoolOffset = atoiOr(value, 0)
	case "poolsize":
		unit.PoolSize = atoiOr(value, 0)
	case "defaultleasetimesec":
		unit.DefaultLeaseTimeSec = value
	case "maxleasetimesec":
		unit.MaxLeaseTimeSec = value
	case "emitdns":
		unit.EmitDNS = boolPtr(parseBool(value, true))
	case "emitrouter":
		unit.EmitRouter = boolPtr(parseBool(value, true))
	case "emitntp":
		unit.EmitNTP = boolPtr(parseBool(value, true))
	case "dns":
		unit.DNS = appendListSetting(unit.DNS, value)
	case "ntp":
		unit.NTP = appendListSetting(unit.NTP, value)
	case "router":
		unit.Router = value
	case "sendoption":
		if domain, ok := parseDomainSendOption(value); ok {
			unit.Domain = domain
		} else if value == "" {
			// An empty SendOption= clears every option sent so far, the
			// domain among them.
			unit.Domain = ""
		}
	}
}

// appendListSetting folds one assignment of a systemd list setting: an empty
// value clears the list, anything else appends its whitespace-separated items.
func appendListSetting(list []string, value string) []string {
	if value == "" {
		return nil
	}
	return append(list, strings.Fields(value)...)
}

// parseDomainSendOption reads a `SendOption=15:string:<domain>` back into the
// domain it sends. Any other option number, type or shape is somebody else's
// line and is not read as a domain.
func parseDomainSendOption(value string) (string, bool) {
	parts := strings.SplitN(value, ":", 3)
	if len(parts) != 3 || parts[0] != domainOptionCode || parts[1] != "string" {
		return "", false
	}
	domain := strings.TrimSpace(parts[2])
	if domain == "" {
		return "", false
	}
	return domain, true
}

// NetworkdModel turns the units found on the machine into the screen's model:
// one pool per unit, every static lease as a reservation, and the advertised
// options of the primary unit.
//
// primary is the unit the mutations target — the first DHCP-serving unit in
// path order. A machine with one LAN has exactly one, which is the case the
// router profile produces; a machine with several still reads them all and
// says which one an edit would touch.
func NetworkdModel(units []NetworkdUnit) dhcp.Model {
	model := dhcp.Model{}
	for _, unit := range units {
		if pool, ok := NetworkdPool(unit); ok {
			model.Pools = append(model.Pools, pool)
		}
		model.Reservations = append(model.Reservations, unit.Leases...)
	}
	if len(units) > 0 {
		model.Options = NetworkdOptions(units[0])
		// The drop-in decides the outcome on its own — it is rendered whole
		// and its scalars beat the unit's — so the editor has to open on what
		// is in effect, not on the drop-in alone. Seeding it from the drop-in
		// would silently drop a DNS= the unit sets the moment the form was
		// submitted unchanged.
		model.OwnOptions = model.Options
	}
	return model
}

// NetworkdPool derives the pool a unit hands out from its address and its
// PoolOffset=/PoolSize=, applying systemd's defaults for the zeros.
func NetworkdPool(unit NetworkdUnit) (dhcp.Pool, bool) {
	start, end, err := PoolRange(unit.Address, unit.PoolOffset, unit.PoolSize)
	if err != nil {
		return dhcp.Pool{}, false
	}
	pool := dhcp.Pool{
		Name:      unit.Link,
		Start:     start,
		End:       end,
		Family:    "ipv4",
		LeaseTime: unit.DefaultLeaseTimeSec,
		Source:    unit.Path,
	}
	if prefix, err := netip.ParsePrefix(unit.Address); err == nil {
		pool.Netmask = netmaskOf(prefix.Bits())
	}
	if !unit.Enabled {
		pool.Mode = "off"
	}
	return pool, true
}

// NetworkdOptions reads what a unit's server advertises: the DNS servers, the
// router, the NTP servers, the domain and the default lease time. An emit
// switch turned off empties its list, because that is what the client is
// handed — nothing.
func NetworkdOptions(unit NetworkdUnit) dhcp.Options {
	o := dhcp.Options{Domain: unit.Domain, LeaseTime: unit.DefaultLeaseTimeSec}
	if emitted(unit.EmitDNS) {
		o.DNS = append([]string{}, unit.DNS...)
	}
	if emitted(unit.EmitNTP) {
		o.NTP = append([]string{}, unit.NTP...)
	}
	if emitted(unit.EmitRouter) {
		o.Gateway = unit.Router
	}
	return o
}

// emitted reads one of the Emit* switches, which default to yes when the unit
// does not mention them.
func emitted(flag *bool) bool { return flag == nil || *flag }

// PoolRange works out the first and last address a [DHCPServer] hands out,
// from the server address and the PoolOffset=/PoolSize= keys.
//
// The arithmetic is systemd's own (sd_dhcp_server_configure_pool): the pool is
// a run of addresses inside the server address's subnet; offset zero means one
// (the address right after the subnet address) and size zero means the rest of
// the subnet up to but not including the broadcast address. The server's own
// address may fall inside the pool — systemd reserves it and hands out the
// rest — so it is not an error here either.
func PoolRange(address string, offset, size int) (start, end string, err error) {
	subnet, hostCount, err := poolSubnet(address)
	if err != nil {
		return "", "", err
	}
	if offset == 0 {
		offset = 1
	}
	// The number of addresses the pool may still take: everything from the
	// offset to the address before the broadcast.
	maxSize := hostCount - offset
	if offset < 1 || maxSize < 1 {
		return "", "", fmt.Errorf(
			"networkd: PoolOffset=%d leaves no address in %s", offset, address)
	}
	if size == 0 {
		size = maxSize
	}
	if size < 1 || size > maxSize {
		return "", "", fmt.Errorf(
			"networkd: PoolSize=%d does not fit in %s after PoolOffset=%d",
			size, address, offset)
	}
	return addrAt(subnet, offset), addrAt(subnet, offset+size-1), nil
}

// PoolOffsetSize is PoolRange backwards: the PoolOffset= and PoolSize= that
// hand out exactly start..end in the server address's subnet. It is what the
// pool-range key writes, and it is where an impossible range is refused — one
// that leaves the subnet, runs backwards, starts on the subnet address or ends
// on the broadcast address, none of which systemd's server would accept.
func PoolOffsetSize(address, start, end string) (offset, size int, err error) {
	subnet, hostCount, err := poolSubnet(address)
	if err != nil {
		return 0, 0, err
	}
	first, err := poolAddr(start)
	if err != nil {
		return 0, 0, err
	}
	last, err := poolAddr(end)
	if err != nil {
		return 0, 0, err
	}
	offset = int(int64(addrToUint(first)) - int64(addrToUint(subnet)))
	span := int(int64(addrToUint(last)) - int64(addrToUint(first)))
	if offset < 1 {
		return 0, 0, fmt.Errorf(
			"networkd: %s is not inside %s above its subnet address",
			start, address)
	}
	if span < 0 {
		return 0, 0, fmt.Errorf("networkd: %s comes before %s", end, start)
	}
	size = span + 1
	if offset+size > hostCount {
		return 0, 0, fmt.Errorf(
			"networkd: %s is not inside %s below its broadcast address",
			end, address)
	}
	return offset, size, nil
}

// poolSubnet splits a server address into the subnet address the offsets are
// measured from and the number of addresses before the broadcast one, which is
// the largest offset+size the pool may reach.
func poolSubnet(address string) (netip.Addr, int, error) {
	prefix, err := netip.ParsePrefix(address)
	if err != nil || !prefix.Addr().Is4() {
		return netip.Addr{}, 0, fmt.Errorf(
			"networkd: %q is not an IPv4 address with a prefix length", address)
	}
	if prefix.Addr().IsUnspecified() {
		return netip.Addr{}, 0, fmt.Errorf(
			"networkd: %s picks its address automatically, so it has no fixed pool",
			address)
	}
	bits := prefix.Bits()
	if bits > 30 {
		return netip.Addr{}, 0, fmt.Errorf(
			"networkd: /%d leaves no address to hand out", bits)
	}
	// broadcastOffset is systemd's own: the host part all ones.
	broadcastOffset := (1 << (32 - bits)) - 1
	return prefix.Masked().Addr(), broadcastOffset, nil
}

// poolAddr parses one bare IPv4 address of a pool range.
func poolAddr(value string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || !addr.Is4() {
		return netip.Addr{}, fmt.Errorf("networkd: %q is not an IPv4 address", value)
	}
	return addr, nil
}

// addrToUint is the address as the number the offsets are counted in.
func addrToUint(addr netip.Addr) uint32 {
	b := addr.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// addrAt is the address `offset` past a subnet address.
func addrAt(subnet netip.Addr, offset int) string {
	value := addrToUint(subnet) + uint32(offset) //nolint:gosec // offset is bounded by the caller to the subnet size
	return uintToAddr(value).String()
}

// uintToAddr is the IPv4 address a 32-bit number spells.
func uintToAddr(value uint32) netip.Addr {
	var octets [4]byte
	binary.BigEndian.PutUint32(octets[:], value)
	return netip.AddrFrom4(octets)
}

// netmaskOf renders a prefix length as the dotted mask the pool line shows.
func netmaskOf(bits int) string {
	if bits < 0 || bits > 32 {
		return ""
	}
	mask := ^uint32(0) << (32 - bits)
	if bits == 0 {
		mask = 0
	}
	return uintToAddr(mask).String()
}

// NetworkdDropinPath is where the drop-in for a unit lands:
// /etc/systemd/network/<unit>.network.d/50-tui-network-dhcp.conf, whichever
// search directory the unit itself was read from.
func NetworkdDropinPath(unitPath string) string {
	return filepath.Join(networkdConfigDir,
		filepath.Base(unitPath)+".d", NetworkdDropinName)
}

// checkNetworkdPath refuses to write anywhere but this tool's own drop-in
// inside a .network.d directory under /etc/systemd/network.
func checkNetworkdPath(path string) error {
	if !strings.HasPrefix(path, networkdConfigDir+"/") {
		return fmt.Errorf("networkd: refusing to write outside %s", networkdConfigDir)
	}
	if filepath.Base(path) != NetworkdDropinName {
		return fmt.Errorf("networkd: a DHCP drop-in must be named %s", NetworkdDropinName)
	}
	if !strings.HasSuffix(filepath.Base(filepath.Dir(path)), networkSuffix+".d") {
		return fmt.Errorf("networkd: a DHCP drop-in belongs in a %s.d directory",
			networkSuffix)
	}
	return nil
}

// NetworkdDropin is everything the drop-in declares: the pool, the lease time,
// what the server advertises, and the reservations. It is rendered whole, so a
// value left out of it is a value the unit underneath still decides.
type NetworkdDropin struct {
	// Link names the interface in the file's header comment.
	Link string
	// PoolOffset and PoolSize are written only when both are non-zero, which
	// is what the pool-range key sets.
	PoolOffset int
	PoolSize   int
	// Options are the advertised values and the default lease time.
	Options dhcp.Options
	// Leases are the static reservations the drop-in owns.
	Leases []dhcp.Reservation
}

// leaseTimeRe is the subset of systemd time spans the lease-time field takes:
// a count of seconds, or a count with one of the usual suffixes. Keeping it
// narrow is deliberate — the value is written into a unit file, and a span
// systemd would reject leaves the server refusing to start.
var networkdLeaseTimeRe = regexp.MustCompile(
	`^[0-9]{1,9}(s|sec|second|seconds|m|min|minute|minutes|h|hr|hour|hours|d|day|days|w|week|weeks)?$`)

// serverAddressToken is the value systemd resolves to the DHCP server's own
// address. It is the right answer for a router — unlike a pinned address it
// survives a renumbering — so the editor accepts and preserves it.
const serverAddressToken = "_server_address"

// RenderNetworkdDropin renders the whole drop-in from a spec. Every address is
// re-parsed and re-printed, the domain must be a domain and the lease time a
// time span, so nothing typed into the form can smuggle another key or another
// section into a systemd unit file.
//
// The list settings are cleared before they are set (`DNS=` then `DNS=…`),
// because a drop-in's list assignment appends to the unit's rather than
// replacing it; without the clear, editing the advertised DNS would leave the
// unit's own servers in the lease as well.
func RenderNetworkdDropin(spec NetworkdDropin) (string, error) {
	var b strings.Builder
	b.WriteString("# Written by tui-network. The DHCP screen owns this file in full:\n")
	if spec.Link != "" {
		fmt.Fprintf(&b, "# the pool, the lease time, the advertised options and the\n"+
			"# static leases of the DHCP server on %s.\n", spec.Link)
	}
	b.WriteString("# systemd-networkd re-reads it on `networkctl reload`.\n\n")
	b.WriteString("[DHCPServer]\n")

	// Both keys are written whenever either is set, so the drop-in decides the
	// pool on its own. Zero is a legal value for both — it is how systemd
	// spells "use the default" — so only a negative one is refused.
	if spec.PoolOffset != 0 || spec.PoolSize != 0 {
		if spec.PoolOffset < 0 || spec.PoolSize < 0 {
			return "", fmt.Errorf("networkd: a pool offset and size cannot be negative")
		}
		fmt.Fprintf(&b, "PoolOffset=%d\n", spec.PoolOffset)
		fmt.Fprintf(&b, "PoolSize=%d\n", spec.PoolSize)
	}

	o := spec.Options
	if o.LeaseTime != "" {
		if !networkdLeaseTimeRe.MatchString(o.LeaseTime) {
			return "", fmt.Errorf(
				"networkd: %q is not a lease time (try 1h, 30min or 3600)", o.LeaseTime)
		}
		fmt.Fprintf(&b, "DefaultLeaseTimeSec=%s\n", o.LeaseTime)
	}

	dns, err := networkdAddressList("advertised DNS server", o.DNS)
	if err != nil {
		return "", err
	}
	writeListSetting(&b, "EmitDNS", "DNS", dns)

	ntp, err := networkdAddressList("advertised NTP server", o.NTP)
	if err != nil {
		return "", err
	}
	writeListSetting(&b, "EmitNTP", "NTP", ntp)

	if o.Gateway != "" {
		router, err := networkdAddressList("advertised gateway", []string{o.Gateway})
		if err != nil {
			return "", err
		}
		b.WriteString("EmitRouter=yes\n")
		fmt.Fprintf(&b, "Router=%s\n", router[0])
	}

	// SendOption= is a list setting too, so the same clear-then-set applies:
	// without it a second edit would leave two option 15 lines behind.
	b.WriteString("SendOption=\n")
	if o.Domain != "" {
		if len(o.Domain) > 253 || !domainRe.MatchString(o.Domain) {
			return "", fmt.Errorf("networkd: %q is not a domain name", o.Domain)
		}
		fmt.Fprintf(&b, "SendOption=%s:string:%s\n", domainOptionCode, o.Domain)
	}

	for _, lease := range spec.Leases {
		if err := checkNetworkdLease(lease); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "\n[DHCPServerStaticLease]\nMACAddress=%s\nAddress=%s\n",
			strings.ToLower(lease.MAC), lease.IP)
	}
	return b.String(), nil
}

// writeListSetting renders one emit switch and its list: the switch off when
// the list is empty and the tool owns the field, the clear-then-set pair when
// there is something to advertise.
func writeListSetting(b *strings.Builder, emitKey, listKey string, values []string) {
	// The clear runs either way: it is what takes the unit's own list out of
	// the lease when the field was emptied in the form.
	fmt.Fprintf(b, "%s=\n", listKey)
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(b, "%s=yes\n", emitKey)
	fmt.Fprintf(b, "%s=%s\n", listKey, strings.Join(values, " "))
}

// networkdAddressList canonicalises a list of advertised servers, keeping the
// _server_address token systemd resolves to the server's own address.
func networkdAddressList(what string, values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if value == serverAddressToken {
			out = append(out, value)
			continue
		}
		ip, ok := parseHost(value)
		if !ok {
			return nil, fmt.Errorf("networkd: %q is not an IP address (%s)", value, what)
		}
		out = append(out, ip)
	}
	return out, nil
}

// checkNetworkdLease refuses a static lease systemd would refuse: both keys are
// mandatory, the address is IPv4 and the MAC is a MAC.
func checkNetworkdLease(lease dhcp.Reservation) error {
	if lease.MAC == "" || !macRe.MatchString(lease.MAC) ||
		strings.Contains(lease.MAC, "*") {
		return fmt.Errorf(
			"networkd: a static lease needs a MAC address, not %q", lease.MAC)
	}
	if _, err := poolAddr(lease.IP); err != nil {
		return fmt.Errorf(
			"networkd: a static lease needs an IPv4 address, not %q", lease.IP)
	}
	return nil
}

// NewNetworkdLease validates a static lease and returns it in the form the
// drop-in stores, refusing a MAC or an address that is already spoken for —
// two [DHCPServerStaticLease] sections for one client is a configuration
// systemd would take and the client would suffer.
//
// existing is every lease the unit hands out, drop-in and unit alike, because
// the clash is with the server's behaviour rather than with one file.
func NewNetworkdLease(existing []dhcp.Reservation,
	add dhcp.Reservation) (dhcp.Reservation, error) {
	if err := checkNetworkdLease(add); err != nil {
		return dhcp.Reservation{}, err
	}
	add.MAC = strings.ToLower(add.MAC)
	for _, lease := range existing {
		if strings.EqualFold(lease.MAC, add.MAC) {
			return dhcp.Reservation{}, fmt.Errorf(
				"networkd: %s already has a static lease (%s)", add.MAC, lease.IP)
		}
		if lease.IP == add.IP {
			return dhcp.Reservation{}, fmt.Errorf(
				"networkd: %s is already reserved for %s", add.IP, lease.MAC)
		}
	}
	add.Family = "ipv4"
	add.Hostname = ""
	return add, nil
}

// RemoveNetworkdLease returns the drop-in's reservations with one gone.
func RemoveNetworkdLease(existing []dhcp.Reservation,
	remove dhcp.Reservation) ([]dhcp.Reservation, error) {
	out := make([]dhcp.Reservation, 0, len(existing))
	found := false
	for _, lease := range existing {
		if !found && matchesLease(lease, remove) {
			found = true
			continue
		}
		out = append(out, lease)
	}
	if !found {
		return nil, fmt.Errorf("networkd: %s is not a static lease this drop-in declares",
			leaseLabel(remove))
	}
	return out, nil
}

// matchesLease reports whether a stored lease is the one a removal names, by
// MAC or by address.
func matchesLease(lease, want dhcp.Reservation) bool {
	if want.MAC != "" && strings.EqualFold(lease.MAC, want.MAC) {
		return true
	}
	return want.IP != "" && lease.IP == want.IP
}

// leaseLabel names a reservation in an error message.
func leaseLabel(lease dhcp.Reservation) string {
	if lease.MAC != "" {
		return lease.MAC
	}
	return lease.IP
}

// leaseLineRe reads one entry of networkctl's "Offered DHCP leases" list:
// `<address> (to <client id>)`, which for an Ethernet client id is the bare
// MAC. There is no hostname and no expiry in that list — systemd does not
// publish them on the DHCPServer bus interface — so a lease read this way
// carries the address and the client, and nothing invented.
var leaseLineRe = regexp.MustCompile(`^([0-9a-fA-F.:]+)\s+\(to\s+(.+)\)$`)

// leasesHeading is the field networkctl prints the DHCP server's leases under.
const leasesHeading = "Offered DHCP leases:"

// ParseNetworkctlLeases reads the leases a link's DHCP server has handed out
// from the text of `networkctl status <link>`. The list is a bus property
// networkctl renders into its table; it is not in the JSON output, so this is
// the read path even on a systemd new enough for --json.
func ParseNetworkctlLeases(out string) []dhcp.Lease {
	var leases []dhcp.Lease
	inList := false
	for _, line := range strings.Split(out, "\n") {
		value := strings.TrimSpace(line)
		if index := strings.Index(line, leasesHeading); index >= 0 {
			inList = true
			value = strings.TrimSpace(line[index+len(leasesHeading):])
		}
		if !inList {
			continue
		}
		match := leaseLineRe.FindStringSubmatch(value)
		if match == nil {
			// "none" and the next field of the table both end the list.
			if value != "" {
				inList = false
			}
			continue
		}
		lease := dhcp.Lease{IP: match[1], Family: familyOf(match[1])}
		client := strings.TrimSpace(match[2])
		if macRe.MatchString(client) {
			lease.MAC = strings.ToLower(client)
		} else {
			lease.ClientID = client
		}
		leases = append(leases, lease)
	}
	return leases
}

// sortNetworkdUnits puts the units in path order, so the primary unit — the
// one a mutation targets — is the same on every read.
func sortNetworkdUnits(units []NetworkdUnit) {
	sort.Slice(units, func(i, j int) bool { return units[i].Path < units[j].Path })
}

// explainNetworkd is the note the screen shows about a networkd DHCP server:
// which unit runs it, and why there is nothing to see when there is not.
func explainNetworkd(units []NetworkdUnit) string {
	if len(units) == 0 {
		return "no .network unit here declares DHCPServer=yes on a subnet of " +
			"its own, so systemd-networkd hands out no leases for a LAN"
	}
	unit := units[0]
	if !unit.Enabled {
		return unit.Path + " has a [DHCPServer] section but DHCPServer=yes is not set, " +
			"so the server is configured and switched off"
	}
	return ""
}

// parseBool reads systemd's boolean spelling, falling back for anything else.
func parseBool(value string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "yes", "true", "on", "1":
		return true
	case "no", "false", "off", "0":
		return false
	default:
		return fallback
	}
}

// boolPtr is the address of a boolean, for the tri-state emit switches.
func boolPtr(b bool) *bool { return &b }

// atoiOr reads an integer, falling back when the value is not one.
func atoiOr(value string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

// firstField is the first whitespace-separated token of a value, which is how
// a Match Name= with several patterns names the first interface.
func firstField(value string) string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// isPrefixedAddress reports whether a value is an IPv4 address with a prefix
// length, which is the only form a DHCP server's subnet can be read from.
func isPrefixedAddress(value string) bool {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return false
	}
	prefix, err := netip.ParsePrefix(fields[0])
	return err == nil && prefix.Addr().Is4()
}

// BuildInstallNetworkdDropin copies a staged drop-in into place. `install -D`
// creates the .network.d directory in the same call, so there is no separate
// mkdir to preview and no window where the file is on disk with the wrong
// permissions.
func BuildInstallNetworkdDropin(tempPath, destination string) (dhcp.Command, error) {
	if err := checkNetworkdPath(destination); err != nil {
		return dhcp.Command{}, err
	}
	return dhcp.Command{
		Argv:        []string{"install", "-D", "-m", FileMode, tempPath, destination},
		Description: fmt.Sprintf("Install %s as %s", tempPath, destination),
		Destructive: true,
	}, nil
}

// BuildReloadNetworkd re-reads the .network files and reconfigures the links
// whose configuration changed, which is what applies a DHCP server change. It
// is not marked destructive: a reload keeps the addresses and the routes that
// are already up, and the leases the server has already handed out stay valid
// until they expire.
func BuildReloadNetworkd() dhcp.Command {
	return dhcp.Command{
		Argv: []string{"networkctl", "reload"},
		Description: "Reload " + networkdUnitName +
			" so it re-reads the DHCP server configuration",
	}
}

// networkdWritePlan assembles the plan for a drop-in write: the diff against
// what is on disk, the staged copy, and the two commands that install it and
// tell networkd to re-read it.
func networkdWritePlan(path, before, after string,
	stage func(path, content string) (string, error)) (dhcp.WritePlan, error) {
	if err := checkNetworkdPath(path); err != nil {
		return dhcp.WritePlan{}, err
	}
	if before == after {
		return dhcp.WritePlan{}, fmt.Errorf("%s already says exactly this", path)
	}
	temp, err := stage(path, after)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	install, err := BuildInstallNetworkdDropin(temp, path)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	return dhcp.WritePlan{
		Path:     path,
		Content:  after,
		Diff:     textdiff.Unified(path, before, after),
		TempPath: temp,
		Commands: []dhcp.Command{install, BuildReloadNetworkd()},
	}, nil
}
