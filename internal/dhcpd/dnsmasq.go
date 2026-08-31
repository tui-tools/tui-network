package dhcpd

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tui-tools/tui-network/internal/dhcp"
)

// The dnsmasq capabilities every dnsmasq backend reports — the real one and the
// fake behind --demo — so a demo behaves exactly like a real run.
var dnsmasqCapabilities = dhcp.Capabilities{
	SupportsAddReservation:    true,
	SupportsRemoveReservation: true,
	SupportsSetPoolRange:      true,
	ManagedFile:               DnsmasqManagedFile,
}

// DnsmasqManagedFile is the drop-in tui-network writes reservations to. A file
// of its own means an added reservation never rewrites the administrator's
// hand-maintained dnsmasq.conf, and a reservation the tool wrote can be told
// from one it did not.
const DnsmasqManagedFile = "/etc/dnsmasq.d/tui-network.conf"

// dnsmasqManagedHeader tops the managed drop-in when the tool creates it, the
// way the .network editor names itself in a file it wrote.
const dnsmasqManagedHeader = "# Written by tui-network. Reservations it adds land here;\n" +
	"# dnsmasq re-reads this on reload (SIGHUP).\n"

// ParseDnsmasqLeases reads the dnsmasq lease file
// (/var/lib/misc/dnsmasq.leases). Each line is
//
//	<expiry> <mac-or-iaid> <ip> <hostname> <client-id>
//
// where expiry is a Unix timestamp in seconds (0 means it never expires), and a
// hostname or client id of "*" means the client offered none. The expiry is
// rendered against now, which the caller passes so the reading is testable.
func ParseDnsmasqLeases(out string, now time.Time) []dhcp.Lease {
	var leases []dhcp.Lease
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		epoch, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		ip, ok := parseHost(fields[2])
		if !ok {
			continue
		}
		lease := dhcp.Lease{
			IP:       ip,
			Hostname: normalizeStar(fields[3]),
			ClientID: normalizeStar(fields[4]),
			Family:   familyOf(ip),
			Expiry:   renderEpochExpiry(epoch, now),
		}
		// The second field is the MAC on a v4 lease and the IAID on a v6 one;
		// only a MAC-shaped value is presented as a MAC.
		if isMACLike(fields[1]) {
			lease.MAC = fields[1]
		} else {
			lease.ClientID = joinNonEmpty(fields[1], lease.ClientID)
		}
		leases = append(leases, lease)
	}
	return leases
}

// rangeModes are the dnsmasq `dhcp-range` keywords that are not addresses or
// lease times: a range carrying one is not an ordinary pool.
var rangeModes = map[string]bool{
	"static": true, "ra-only": true, "ra-names": true, "ra-stateless": true,
	"slaac": true, "ra-advrouter": true, "off-link": true, "ra-default-router": true,
}

// leaseTimeRe matches a dnsmasq lease time ("12h", "45m", "infinite",
// "deprecated"), which a range or host line may carry as its last field.
var leaseTimeRe = regexp.MustCompile(`^([0-9]+[smhd]?|infinite|deprecated)$`)

// netmaskRe matches a dotted IPv4 netmask, which a v4 dhcp-range may carry
// between its addresses and its lease time.
var netmaskRe = regexp.MustCompile(`^(255|254|252|248|240|224|192|128|0)(\.(255|254|252|248|240|224|192|128|0)){3}$`)

// ParseDnsmasqConf reads dnsmasq configuration text into the pools and
// reservations it declares. path names the file the settings came from, so a
// later edit rewrites the right one; the raw form dnsmasq also accepts on the
// command line ("--dhcp-range=...") is tolerated by trimming the dashes.
func ParseDnsmasqConf(path, raw string) (pools []dhcp.Pool, reservations []dhcp.Reservation) {
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := splitConfLine(line)
		if !ok {
			continue
		}
		switch key {
		case "dhcp-range":
			if pool, ok := parseDHCPRange(path, value); ok {
				pools = append(pools, pool)
			}
		case "dhcp-host":
			if res, ok := parseDHCPHost(path, value); ok {
				reservations = append(reservations, res)
			}
		}
	}
	return pools, reservations
}

// splitConfLine splits one configuration line into its key and value, dropping
// comments, blank lines and the leading dashes of a command-line form.
func splitConfLine(line string) (key, value string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	trimmed = strings.TrimLeft(trimmed, "-")
	k, v, found := strings.Cut(trimmed, "=")
	if !found {
		return "", "", false
	}
	return strings.TrimSpace(k), strings.TrimSpace(v), true
}

// parseDHCPRange reads the value of a `dhcp-range=` line. The grammar it copes
// with is the common one: optional leading tag:/set: scopes, one or two
// addresses, then any of a netmask, a prefix length, a mode keyword and a lease
// time, in any order dnsmasq accepts.
func parseDHCPRange(path, value string) (dhcp.Pool, bool) {
	pool := dhcp.Pool{Source: path}
	var names []string
	for _, token := range splitTrim(value) {
		switch {
		case hasScopePrefix(token):
			names = append(names, token)
		case pool.Start == "" && isAddr(token):
			pool.Start = stripBrackets(token)
			pool.Family = familyOf(pool.Start)
		case pool.End == "" && isAddr(token):
			pool.End = stripBrackets(token)
			if pool.Family == "" {
				pool.Family = familyOf(pool.End)
			}
		case netmaskRe.MatchString(token):
			pool.Netmask = token
		case isPrefixLen(token):
			pool.PrefixLen, _ = strconv.Atoi(token)
		case rangeModes[token]:
			pool.Mode = token
		case leaseTimeRe.MatchString(token):
			pool.LeaseTime = token
		}
	}
	if pool.Start == "" {
		return dhcp.Pool{}, false
	}
	pool.Name = strings.Join(names, ",")
	return pool, true
}

// parseDHCPHost reads the value of a `dhcp-host=` line into a reservation. The
// tokens can arrive in any order, so each is classified: a MAC, an `id:` client
// id, an address, a lease time, or — what is left — the hostname.
func parseDHCPHost(path, value string) (dhcp.Reservation, bool) {
	res := dhcp.Reservation{Source: path}
	var macs []string
	for _, token := range splitTrim(value) {
		switch {
		case isMACLike(token):
			macs = append(macs, token)
		case strings.HasPrefix(token, "id:"):
			res.ClientID = strings.TrimPrefix(token, "id:")
		case hasScopePrefix(token):
			// A tag assignment: kept in the file on rewrite, not shown.
		case isAddr(token):
			res.IP = stripBrackets(token)
			res.Family = familyOf(res.IP)
		case leaseTimeRe.MatchString(token) || token == "ignore":
			// A lease time or the ignore flag: not part of the identity shown.
		default:
			if res.Hostname == "" {
				res.Hostname = token
			}
		}
	}
	res.MAC = strings.Join(macs, ",")
	if res.MAC == "" && res.ClientID == "" && res.IP == "" {
		return dhcp.Reservation{}, false
	}
	if res.Family == "" {
		res.Family = "ipv4"
	}
	return res, true
}

// RenderReservationLine renders the canonical `dhcp-host=` line for a
// reservation the tool adds: the MAC, the address, and the hostname when there
// is one.
func RenderReservationLine(r dhcp.Reservation) (string, error) {
	if err := checkReservation(r); err != nil {
		return "", err
	}
	parts := []string{r.MAC}
	if r.IP != "" {
		parts = append(parts, r.IP)
	}
	if r.Hostname != "" {
		parts = append(parts, r.Hostname)
	}
	return "dhcp-host=" + strings.Join(parts, ","), nil
}

// AddReservationText returns the managed file's text with a reservation
// appended. A file the tool has not written yet gets its header first.
func AddReservationText(existing string, r dhcp.Reservation) (string, error) {
	line, err := RenderReservationLine(r)
	if err != nil {
		return "", err
	}
	if lineExists(existing, line) {
		return "", fmt.Errorf("dnsmasq: %s already reserves that", DnsmasqManagedFile)
	}
	var b strings.Builder
	if strings.TrimSpace(existing) == "" {
		b.WriteString(dnsmasqManagedHeader)
	} else {
		b.WriteString(strings.TrimRight(existing, "\n"))
		b.WriteString("\n")
	}
	b.WriteString(line)
	b.WriteString("\n")
	return b.String(), nil
}

// RemoveReservationText returns the file's text with the reservation's
// `dhcp-host=` line dropped. The line is matched by the MAC or the address the
// reservation carries, so the same reservation read back out identifies its own
// line.
func RemoveReservationText(existing string, r dhcp.Reservation) (string, error) {
	lines := strings.Split(existing, "\n")
	kept := make([]string, 0, len(lines))
	removed := false
	for _, line := range lines {
		key, value, ok := splitConfLine(line)
		if ok && key == "dhcp-host" && reservationMatches(value, r) {
			removed = true
			continue
		}
		kept = append(kept, line)
	}
	if !removed {
		return "", fmt.Errorf("dnsmasq: no matching dhcp-host line to remove")
	}
	return strings.Join(kept, "\n"), nil
}

// SetPoolRangeText returns the file's text with the range that currently reads
// orig rewritten to newStart..newEnd. The addresses are replaced in place, so a
// range's netmask, lease time and tags survive the edit.
func SetPoolRangeText(existing string, orig dhcp.Pool, newStart, newEnd string) (string, error) {
	if !isAddr(newStart) || !isAddr(newEnd) {
		return "", fmt.Errorf("dnsmasq: a pool range needs two addresses, got %q and %q",
			newStart, newEnd)
	}
	if familyOf(newStart) != familyOf(newEnd) {
		return "", fmt.Errorf("dnsmasq: the two addresses are not the same family")
	}
	lines := strings.Split(existing, "\n")
	changed := false
	for i, line := range lines {
		key, value, ok := splitConfLine(line)
		if !ok || key != "dhcp-range" {
			continue
		}
		pool, ok := parseDHCPRange(orig.Source, value)
		if !ok || pool.Start != orig.Start || pool.End != orig.End {
			continue
		}
		lines[i] = replaceRangeAddresses(line, orig.Start, orig.End, newStart, newEnd)
		changed = true
		break
	}
	if !changed {
		return "", fmt.Errorf("dnsmasq: no dhcp-range matching %s..%s to adjust",
			orig.Start, orig.End)
	}
	return strings.Join(lines, "\n"), nil
}

// replaceRangeAddresses swaps the two address tokens of a dhcp-range line,
// leaving everything else — the key, the tags, the netmask, the lease time —
// exactly as it was.
func replaceRangeAddresses(line, oldStart, oldEnd, newStart, newEnd string) string {
	line = replaceToken(line, oldStart, newStart)
	if oldEnd != "" {
		line = replaceToken(line, oldEnd, newEnd)
	}
	return line
}

// replaceToken replaces the first comma- or equals-delimited occurrence of old
// with replacement, so "192.0.2.50" is swapped without touching a longer token
// it is a prefix of.
func replaceToken(line, old, replacement string) string {
	for _, sep := range []string{"=", ","} {
		needle := sep + old
		if i := strings.Index(line, needle); i >= 0 {
			end := i + len(needle)
			if end == len(line) || line[end] == ',' || line[end] == ' ' {
				return line[:i+len(sep)] + replacement + line[end:]
			}
		}
	}
	return strings.Replace(line, old, replacement, 1)
}

// reservationMatches reports whether a dhcp-host value is the reservation r: the
// same MAC, or the same address when r has no MAC.
func reservationMatches(value string, r dhcp.Reservation) bool {
	parsed, ok := parseDHCPHost("", value)
	if !ok {
		return false
	}
	if r.MAC != "" {
		return strings.EqualFold(parsed.MAC, r.MAC)
	}
	if r.ClientID != "" {
		return parsed.ClientID == r.ClientID
	}
	return r.IP != "" && parsed.IP == r.IP
}

// macRe matches a hardware address, allowing the wildcard octets dnsmasq lets a
// dhcp-host use.
var macRe = regexp.MustCompile(`^([0-9A-Fa-f]{1,2}|\*)(:([0-9A-Fa-f]{1,2}|\*)){5}$`)

// checkReservation rejects a reservation the tool would refuse to write: one
// with no MAC, or with an address that is not an address.
func checkReservation(r dhcp.Reservation) error {
	if !macRe.MatchString(r.MAC) {
		return fmt.Errorf("dnsmasq: %q is not a MAC address", r.MAC)
	}
	if r.IP != "" && !isAddr(r.IP) {
		return fmt.Errorf("dnsmasq: %q is not an IP address", r.IP)
	}
	if strings.ContainsAny(r.Hostname, ", \t") {
		return fmt.Errorf("dnsmasq: %q is not a hostname", r.Hostname)
	}
	return nil
}

// --- small shared helpers -------------------------------------------------

// splitTrim splits a comma list and trims each element, dropping empties.
func splitTrim(value string) []string {
	var out []string
	for _, token := range strings.Split(value, ",") {
		token = strings.TrimSpace(token)
		if token != "" {
			out = append(out, token)
		}
	}
	return out
}

// hasScopePrefix reports whether a token is a dnsmasq tag or set scope.
func hasScopePrefix(token string) bool {
	return strings.HasPrefix(token, "tag:") || strings.HasPrefix(token, "set:")
}

// stripBrackets removes the [ ] a dnsmasq config puts around an IPv6 address.
func stripBrackets(token string) string {
	return strings.TrimSuffix(strings.TrimPrefix(token, "["), "]")
}

// isAddr reports whether a token is an IP address, brackets and all.
func isAddr(token string) bool {
	_, ok := parseHost(token)
	return ok
}

// parseHost parses an address, tolerating the [v6] bracket form, and returns
// its canonical text. A scope zone ("fe80::1%eth0") is dropped: a lease or a
// pool address carries no zone, and keeping one would put a "%" — and whatever
// followed it, spaces included — into a value the UI prints and a command
// builder validates.
func parseHost(token string) (string, bool) {
	addr, err := netip.ParseAddr(stripBrackets(token))
	if err != nil {
		return "", false
	}
	return addr.WithZone("").String(), true
}

// familyOf names the family of an address, empty when it is not one.
func familyOf(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	if addr.Is4() {
		return "ipv4"
	}
	return "ipv6"
}

// isPrefixLen reports whether a token is an IPv6 prefix length (1..128).
func isPrefixLen(token string) bool {
	n, err := strconv.Atoi(token)
	return err == nil && n >= 1 && n <= 128
}

// isMACLike reports whether a token looks like a MAC, wildcards allowed.
func isMACLike(token string) bool {
	return macRe.MatchString(token)
}

// normalizeStar turns dnsmasq's "*" placeholder into an empty value.
func normalizeStar(s string) string {
	if s == "*" {
		return ""
	}
	return s
}

// joinNonEmpty joins two values with a space, dropping either if empty.
func joinNonEmpty(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + " " + b
	}
}

// lineExists reports whether a rendered line is already present, ignoring
// surrounding whitespace.
func lineExists(text, line string) bool {
	for _, existing := range strings.Split(text, "\n") {
		if strings.TrimSpace(existing) == line {
			return true
		}
	}
	return false
}

// renderEpochExpiry turns a Unix-second lease expiry into human text against
// now. Zero means the lease never expires, which dnsmasq writes for an infinite
// lease.
func renderEpochExpiry(epoch int64, now time.Time) string {
	if epoch == 0 {
		return "never"
	}
	return renderExpiry(time.Unix(epoch, 0), now)
}
