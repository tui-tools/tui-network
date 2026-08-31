package main

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/tui-tools/tui-kit/ui"
	"github.com/tui-tools/tui-network/internal/network"
)

// gatewayHeight is the number of uplink rows that fit on screen. It matches the
// links table's budget: header, table header, footer and status line.
func (a *app) gatewayHeight() int {
	return max(a.height-headerLines-footerLines-2, minTableHeight)
}

// gatewaysView renders the router's Gateways screen: the uplinks the machine
// has, which one is the default now, and — through the footer keys — the switch
// and the manual failover. It is a selectable table, like the links overview,
// because the action keys apply to the highlighted uplink.
func (a *app) gatewaysView() string {
	header := a.gatewaysHeaderView()

	var body string
	switch {
	case !a.gatewaysLoaded:
		body = ui.EmptyState(a.theme, "reading the uplinks…", a.width, a.gatewayHeight()+1)
	case len(a.gateways) == 0:
		body = ui.EmptyState(a.theme,
			"no default route here — a router has at least one uplink",
			a.width, a.gatewayHeight()+1)
	default:
		body = a.gatewaysTable()
	}

	help := ui.HelpBar(a.theme, a.gatewaysHelpKeys(), a.width)
	status := ui.StatusLine(a.theme, a.statusKind, a.status, a.gatewaysStatus(), a.width)
	return strings.Join([]string{header, body, help, status}, "\n")
}

// gatewaysHeaderView renders the facts at the top of the Gateways screen: how
// many uplinks there are, which one is the default, and whether a failover is
// even possible.
func (a *app) gatewaysHeaderView() string {
	t := a.theme
	facts := []ui.Fact{{Label: "uplinks", Value: strconv.Itoa(len(a.gateways))}}

	if active, ok := a.activeGateway(); ok {
		style := t.OK
		facts = append(facts, ui.Fact{Label: "default",
			Value: active.Interface + " via " + active.Address, Style: &style})
	} else if a.gatewaysLoaded {
		none := t.Muted
		facts = append(facts, ui.Fact{Label: "default", Value: "none", Style: &none})
	}
	if _, ok := network.Standby(a.gateways); ok {
		style := t.OK
		facts = append(facts, ui.Fact{Label: "failover", Value: "available", Style: &style})
	}

	return ui.Header{Title: "tui-network — Gateways",
		Subtitle: a.backend.Describe(), Facts: facts}.Render(t, a.width)
}

// activeGateway returns the uplink the kernel is using now, if there is one.
func (a *app) activeGateway() (network.Gateway, bool) {
	for _, gw := range a.gateways {
		if gw.Active {
			return gw, true
		}
	}
	return network.Gateway{}, false
}

// gatewaysTable renders the uplink list, dropping columns on narrow terminals.
func (a *app) gatewaysTable() string {
	columns := []ui.Column{
		{Title: "INTERFACE", Width: 12, Flex: true},
		{Title: "GATEWAY", Width: 16, Flex: true},
		{Title: "METRIC", Width: 7},
		{Title: "ROLE", Width: 9},
	}
	showFamily := a.width >= 64
	showSource := a.width >= 82
	showReach := a.width >= 100
	if showFamily {
		columns = append(columns, ui.Column{Title: "FAMILY", Width: 6})
	}
	if showSource {
		columns = append(columns, ui.Column{Title: "SOURCE", Width: 12})
	}
	if showReach {
		columns = append(columns, ui.Column{Title: "REACH", Width: 12})
	}

	rows := make([][]string, 0, len(a.gateways))
	styles := make([]*lipgloss.Style, 0, len(a.gateways))
	for _, gw := range a.gateways {
		row := []string{
			gw.Interface,
			gw.Address,
			strconv.Itoa(gw.Metric),
			gatewayRole(gw),
		}
		if showFamily {
			row = append(row, gw.Family)
		}
		if showSource {
			row = append(row, gatewaySource(gw))
		}
		if showReach {
			row = append(row, gatewayReach(gw))
		}
		rows = append(rows, row)
		styles = append(styles, a.gatewayStyle(gw))
	}

	return ui.Table{
		Columns:  columns,
		Rows:     rows,
		Styles:   styles,
		Selected: a.gatewayCursor,
		Offset:   a.gatewayOffset,
		Height:   a.gatewayHeight(),
	}.Render(a.theme, a.width)
}

// gatewayRole names an uplink's place: the default in use, one leg of a
// balanced default, or a standby waiting to be promoted.
func gatewayRole(g network.Gateway) string {
	switch {
	case g.Active && g.Multipath:
		return "balanced"
	case g.Active:
		return "active"
	default:
		return "standby"
	}
}

// gatewaySource says where the uplink's priority is anchored: a persistent
// .network setting, or only the live routing table, and whether tui-network may
// make it persistent at all.
func gatewaySource(g network.Gateway) string {
	switch {
	case g.Persistent:
		return "persistent"
	case g.Managed:
		return "live"
	default:
		return "unmanaged"
	}
}

// gatewayReach renders the reachability probe's verdict, blank until it runs.
func gatewayReach(g network.Gateway) string {
	switch {
	case g.Egress.Dev == "":
		return ""
	case g.Reachable():
		return "reachable"
	default:
		return "via " + g.Egress.Dev
	}
}

// gatewayStyle colours a row by its role and reachability: the active default
// stands out, and an uplink the kernel cannot reach on its own interface is
// flagged.
func (a *app) gatewayStyle(g network.Gateway) *lipgloss.Style {
	var style lipgloss.Style
	switch {
	case g.Egress.Dev != "" && !g.Reachable():
		style = a.theme.Row.Foreground(a.theme.Warn.GetForeground())
	case g.Active:
		style = a.theme.Row.Foreground(a.theme.OK.GetForeground())
	default:
		style = a.theme.Row
	}
	return &style
}

// gatewaysStatus is the hint shown when there is no message to report.
func (a *app) gatewaysStatus() string {
	if len(a.gateways) == 0 {
		return "esc to go back  ·  ? for help"
	}
	return strconv.Itoa(len(a.gateways)) + " uplinks  ·  s set default  ·  " +
		"x failover  ·  esc back"
}

// gatewaysHelpKeys is the hint bar of the Gateways screen. The persist key
// appears only for a managed uplink that can take a drop-in, so an unmanaged
// selection does not advertise a change it will refuse.
func (a *app) gatewaysHelpKeys() []ui.KeyHint {
	hints := []ui.KeyHint{
		{Key: "s", Desc: "set default"},
		{Key: "x", Desc: "failover"},
	}
	if gw, ok := a.currentGateway(); ok && gw.Managed && gw.ConfigFile != "" {
		hints = append(hints, ui.KeyHint{Key: "P", Desc: "persist"})
	}
	return append(hints,
		ui.KeyHint{Key: "R", Desc: "reload"},
		ui.KeyHint{Key: "esc", Desc: "back"})
}
