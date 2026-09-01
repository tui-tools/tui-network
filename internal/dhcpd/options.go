package dhcpd

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tui-tools/tui-network/internal/dhcp"
)

// DnsmasqOptionsFile is the drop-in the options editor rewrites: what DHCP
// advertises (DNS servers, gateway, domain) and the upstream `server=`
// forwarders. Like the reservations drop-in, it is a file of the tool's own —
// the editor regenerates it wholesale and never touches a line in a file the
// administrator maintains. The 50- prefix keeps it apart from the reservations
// file and gives it a stable place in dnsmasq's alphabetical read order.
const DnsmasqOptionsFile = "/etc/dnsmasq.d/50-tui-network.conf"

// dnsmasqOptionsHeader tops the options drop-in, naming its owner and how it
// is applied. dnsmasq does not re-read configuration files on SIGHUP, so a
// change here needs a restart, and the header says so.
const dnsmasqOptionsHeader = "# Written by tui-network. The o key on the DHCP screen edits this file\n" +
	"# in full: advertised DHCP options and upstream DNS forwarders.\n" +
	"# Applied with a restart; dnsmasq does not re-read config files on SIGHUP.\n"

// optionCodes maps the dhcp-option names dnsmasq also accepts onto the numeric
// codes this tool reads and writes.
var optionCodes = map[string]string{
	"option:router":     "3",
	"option:dns-server": "6",
}

// ParseDnsmasqOptions reads one configuration file's advertised options and
// upstream forwarders: untagged `dhcp-option=3,…` and `dhcp-option=6,…` lines
// (numeric or option:name form), `domain=`, and plain `server=<ip>`
// forwarders. Tag-scoped options belong to a client class, not to the pool at
// large, and `server=/domain/ip` routes one zone rather than naming an
// upstream, so both are left alone — parsed by nobody, owned by the
// administrator.
func ParseDnsmasqOptions(path, raw string) dhcp.Options {
	_ = path // reserved: options render with no per-line source today
	var o dhcp.Options
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := splitConfLine(line)
		if !ok {
			continue
		}
		switch key {
		case "dhcp-option", "dhcp-option-force":
			code, values, ok := parseDHCPOptionValue(value)
			if !ok {
				continue
			}
			switch code {
			case "3":
				if len(values) > 0 {
					o.Gateway = values[0]
				}
			case "6":
				// A later option 6 line replaces an earlier one, the way a
				// client only ever receives one set of servers. An empty
				// value list still marks the option as set (non-nil).
				o.DNS = append([]string{}, values...)
			}
		case "domain":
			// domain= may carry a range after the name; the name is first.
			if tokens := splitTrim(value); len(tokens) > 0 {
				o.Domain = tokens[0]
			}
		case "server":
			if ip, ok := parseHost(value); ok {
				o.Upstreams = append(o.Upstreams, ip)
			}
		}
	}
	return o
}

// parseDHCPOptionValue splits a dhcp-option value into its option code and
// address list. A tag-scoped option (tag:…) is not the pool-wide value this
// tool edits, and a value that is not a clean list of addresses is a form the
// tool does not own; both report !ok.
func parseDHCPOptionValue(value string) (code string, addrs []string, ok bool) {
	tokens := splitTrim(value)
	if len(tokens) == 0 {
		return "", nil, false
	}
	for _, token := range tokens {
		if hasScopePrefix(token) {
			return "", nil, false
		}
	}
	code = tokens[0]
	if named, found := optionCodes[code]; found {
		code = named
	}
	addrs = make([]string, 0, len(tokens)-1)
	for _, token := range tokens[1:] {
		ip, isAddr := parseHost(token)
		if !isAddr {
			return "", nil, false
		}
		addrs = append(addrs, ip)
	}
	return code, addrs, true
}

// MergeOptions folds a later file's options over an earlier one's, mirroring
// how dnsmasq resolves them across its read order: a scalar set later wins, a
// later option 6 list replaces the earlier one, and `server=` forwarders
// accumulate — dnsmasq uses them all.
func MergeOptions(base, next dhcp.Options) dhcp.Options {
	if next.DNS != nil {
		base.DNS = next.DNS
	}
	if next.Gateway != "" {
		base.Gateway = next.Gateway
	}
	if next.Domain != "" {
		base.Domain = next.Domain
	}
	for _, up := range next.Upstreams {
		if !containsString(base.Upstreams, up) {
			base.Upstreams = append(base.Upstreams, up)
		}
	}
	return base
}

// domainRe matches a plain DNS domain: letter-or-digit labels joined by dots,
// hyphens inside a label only. It is the gate that keeps a shell
// metacharacter, a comma or a space out of a rendered `domain=` line.
var domainRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?` +
	`(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// RenderOptionsFile renders the whole options drop-in from o. Every address is
// re-parsed and re-printed canonically and the domain must match domainRe, so
// nothing a user typed can smuggle a newline, a comma or another directive
// into the file.
//
// An empty DNS list or gateway renders nothing rather than a
// dhcp-option=6,<router-ip> line: with the option absent dnsmasq advertises
// its own address as DNS server (option 6) and router (option 3), which is
// exactly what a dnsmasq router wants — and unlike a pinned address it stays
// right if the router is ever renumbered (man dnsmasq, --dhcp-option).
func RenderOptionsFile(o dhcp.Options) (string, error) {
	var b strings.Builder
	b.WriteString(dnsmasqOptionsHeader)

	if len(o.DNS) > 0 {
		ips, err := canonicalIPs("advertised DNS server", o.DNS)
		if err != nil {
			return "", err
		}
		b.WriteString("dhcp-option=6," + strings.Join(ips, ",") + "\n")
	}
	if o.Gateway != "" {
		ips, err := canonicalIPs("advertised gateway", []string{o.Gateway})
		if err != nil {
			return "", err
		}
		b.WriteString("dhcp-option=3," + ips[0] + "\n")
	}
	if o.Domain != "" {
		if len(o.Domain) > 253 || !domainRe.MatchString(o.Domain) {
			return "", fmt.Errorf("dnsmasq: %q is not a domain name", o.Domain)
		}
		b.WriteString("domain=" + o.Domain + "\n")
	}
	for _, ip := range o.Upstreams {
		ips, err := canonicalIPs("upstream DNS server", []string{ip})
		if err != nil {
			return "", err
		}
		b.WriteString("server=" + ips[0] + "\n")
	}
	return b.String(), nil
}

// canonicalIPs parses each value as an address and returns the canonical
// texts, or says which field refused which value.
func canonicalIPs(what string, values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, value := range values {
		ip, ok := parseHost(value)
		if !ok {
			return nil, fmt.Errorf("dnsmasq: %q is not an IP address (%s)", value, what)
		}
		out = append(out, ip)
	}
	return out, nil
}

// containsString reports whether list holds s.
func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
