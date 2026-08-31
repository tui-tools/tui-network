package main

import (
	"strconv"
	"strings"

	"github.com/tui-tools/tui-kit/ui"
	"github.com/tui-tools/tui-network/internal/dhcp"
)

// dhcpView renders the router's DHCP screen: the server, the pools it hands out,
// the reservations that pin a host to an address, and the leases it has granted.
// It is a scrollable text view, like the link detail screen, so one place
// handles the scrolling and the width.
func (a *app) dhcpView() string {
	header := a.dhcpHeaderView()
	lines := a.dhcpLines()

	height := a.detailHeight()
	offset := min(a.dhcpOffset, max(len(lines)-height, 0))
	a.dhcpOffset = offset
	end := min(offset+height, len(lines))

	body := make([]string, 0, height)
	for _, line := range lines[offset:end] {
		body = append(body, a.theme.Row.Width(a.width).Render(
			ui.Truncate(line, a.width-2)))
	}
	for i := len(body); i < height; i++ {
		body = append(body, a.theme.Row.Width(a.width).Render(""))
	}

	help := ui.HelpBar(a.theme, a.dhcpHelpKeys(), a.width)
	position := strconv.Itoa(offset+1) + "–" + strconv.Itoa(end) +
		" of " + strconv.Itoa(len(lines)) + " lines  ·  esc to go back"
	status := ui.StatusLine(a.theme, a.statusKind, a.status, position, a.width)
	return strings.Join([]string{header,
		strings.Join(body, "\n"), help, status}, "\n")
}

// dhcpHeaderView renders the facts at the top of the DHCP screen.
func (a *app) dhcpHeaderView() string {
	t := a.theme
	server := a.dhcpModel.Server

	var facts []ui.Fact
	switch server.Kind {
	case dhcp.KindDnsmasq, dhcp.KindKea:
		state, style := "active", t.OK
		if !server.Active {
			state, style = "inactive", t.Warn
		}
		facts = append(facts,
			ui.Fact{Label: "server", Value: server.Kind},
			ui.Fact{Label: "state", Value: state, Style: &style})
		if server.CombinedDNS {
			dns := t.OK
			facts = append(facts, ui.Fact{Label: "dns", Value: "served here", Style: &dns})
		}
		facts = append(facts,
			ui.Fact{Label: "pools", Value: strconv.Itoa(len(a.dhcpModel.Pools))},
			ui.Fact{Label: "leases", Value: strconv.Itoa(len(a.dhcpModel.Leases))})
	default:
		none := t.Muted
		facts = append(facts, ui.Fact{Label: "server", Value: "none", Style: &none})
	}
	if a.dhcpCompat.Backend != "" {
		facts = append(facts, ui.CompatFact(t, a.dhcpCompat))
	}

	return ui.Header{Title: "tui-network — DHCP", Subtitle: a.dhcp.Describe(), Facts: facts}.
		Render(t, a.width)
}

// dhcpLines builds the DHCP screen's text, section by section.
func (a *app) dhcpLines() []string {
	if !a.dhcpLoaded {
		return []string{"", "  reading the DHCP server…"}
	}
	server := a.dhcpModel.Server
	if server.Kind == dhcp.KindNone {
		return []string{"", "  " + orNone(server.Explain),
			"",
			"  A router serves DHCP and local DNS to its LAN. Install dnsmasq or",
			"  ISC Kea and this screen fills in.",
			"",
			"  Run tui-network --demo to see it with a sample dnsmasq."}
	}

	lines := []string{"Server"}
	lines = append(lines,
		"  kind           "+server.Kind+versionSuffix(server.Version),
		"  running        "+yesNo(server.Active))
	if server.CombinedDNS {
		lines = append(lines,
			"  dns            dnsmasq answers DNS and DHCP from one process; "+
				"the links screen's resolver view is only systemd-resolved")
	}
	for _, path := range server.ConfPaths {
		lines = append(lines, "  config         "+path)
	}
	if server.LeasesPath != "" {
		lines = append(lines, "  leases file    "+server.LeasesPath)
	}
	if server.ManagedFile != "" {
		lines = append(lines, "  tui-network    writes reservations to "+server.ManagedFile)
	}
	if server.Explain != "" {
		lines = append(lines, "  note           "+server.Explain)
	}

	lines = append(lines, "", "Pools")
	if len(a.dhcpModel.Pools) == 0 {
		lines = append(lines, "  (none)")
	}
	for _, pool := range a.dhcpModel.Pools {
		lines = append(lines, "  "+poolLine(pool))
	}

	lines = append(lines, "", "Reservations")
	if len(a.dhcpModel.Reservations) == 0 {
		lines = append(lines, "  (none)")
	}
	for _, res := range a.dhcpModel.Reservations {
		lines = append(lines, "  "+reservationLine(res))
	}

	lines = append(lines, "", "Leases")
	if len(a.dhcpModel.Leases) == 0 {
		lines = append(lines, "  (none)")
	}
	for _, lease := range a.dhcpModel.Leases {
		lines = append(lines, "  "+leaseLine(lease))
	}
	return lines
}

// poolLine renders one pool: its range, then the scope, netmask, lease time and
// mode when it has them.
func poolLine(p dhcp.Pool) string {
	span := p.Start
	if p.End != "" {
		span += "–" + p.End
	}
	parts := []string{span}
	if p.Name != "" {
		parts = append(parts, p.Name)
	}
	if p.Netmask != "" {
		parts = append(parts, "mask "+p.Netmask)
	}
	if p.PrefixLen > 0 {
		parts = append(parts, "/"+strconv.Itoa(p.PrefixLen))
	}
	if p.LeaseTime != "" {
		parts = append(parts, "lease "+p.LeaseTime)
	}
	if p.Mode != "" {
		parts = append(parts, p.Mode)
	}
	return strings.Join(parts, "  ")
}

// reservationLine renders one reservation: the client it matches, the address
// it pins, and the hostname when there is one.
func reservationLine(r dhcp.Reservation) string {
	who := r.MAC
	if who == "" {
		who = "id:" + r.ClientID
	}
	parts := []string{who}
	if r.IP != "" {
		parts = append(parts, "→ "+r.IP)
	}
	if r.Hostname != "" {
		parts = append(parts, "("+r.Hostname+")")
	}
	return strings.Join(parts, "  ")
}

// leaseLine renders one lease: the client, its address, its hostname and how
// long the lease has left.
func leaseLine(l dhcp.Lease) string {
	who := l.MAC
	if who == "" {
		who = orNone(l.ClientID)
	}
	parts := []string{ui.Pad(who, 17), ui.Pad(l.IP, 15)}
	name := l.Hostname
	if name == "" {
		name = "—"
	}
	parts = append(parts, ui.Pad(name, 16), l.Expiry)
	return strings.Join(parts, " ")
}

// versionSuffix renders " 2.90" after the server kind, empty when unknown.
func versionSuffix(version string) string {
	if version == "" {
		return ""
	}
	return " " + version
}

// yesNo renders a boolean as a word.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// dhcpHelpKeys is the hint bar of the DHCP screen. The mutation keys appear only
// when the detected backend offers them, so a Kea or serverless machine does not
// advertise a change it will refuse.
func (a *app) dhcpHelpKeys() []ui.KeyHint {
	hints := []ui.KeyHint{}
	if a.dhcpCaps.SupportsAddReservation {
		hints = append(hints, ui.KeyHint{Key: "a", Desc: "add reservation"})
	}
	if a.dhcpCaps.SupportsRemoveReservation {
		hints = append(hints, ui.KeyHint{Key: "x", Desc: "remove reservation"})
	}
	if a.dhcpCaps.SupportsSetPoolRange {
		hints = append(hints, ui.KeyHint{Key: "p", Desc: "pool range"})
	}
	return append(hints,
		ui.KeyHint{Key: "R", Desc: "reload"},
		ui.KeyHint{Key: "esc", Desc: "back"})
}
