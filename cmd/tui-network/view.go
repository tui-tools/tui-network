package main

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/tui-tools/tui-kit/ui"
	"github.com/tui-tools/tui-network/internal/network"
)

// Layout constants: the rows the table cannot use.
const (
	headerLines = 2
	footerLines = 2
	// minTableHeight keeps at least one visible row on a very short terminal.
	minTableHeight = 1
)

// tableHeight is the number of link rows that fit on screen.
func (a *app) tableHeight() int {
	// header + table header + footer + status line.
	return max(a.height-headerLines-footerLines-2, minTableHeight)
}

// detailHeight is the number of detail lines that fit on screen.
func (a *app) detailHeight() int {
	return max(a.height-headerLines-footerLines-1, minTableHeight)
}

// View renders the whole screen.
func (a *app) View() string {
	switch a.mode {
	case modeConfirm:
		return a.confirm.View(a.theme, a.width, a.height)
	case modeFilter, modeInput:
		return a.input.View(a.theme, a.width, a.height)
	case modePicker:
		return a.picker.View(a.theme, a.width, a.height)
	case modeForm:
		return a.form.view(a.theme, a.width, a.height)
	case modeHelp:
		return placeCenter(
			ui.HelpScreen(a.theme, "tui-network — keys", helpKeys(), a.width),
			a.width, a.height)
	case modeDetail:
		return a.detailView()
	}
	return a.linksView()
}

// placeCenter centers a rendered box in the terminal.
func placeCenter(box string, width, height int) string {
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}

// linksView renders the overview: header, link table, help bar, status.
func (a *app) linksView() string {
	header := a.headerView("")

	var body string
	switch {
	case a.loading && len(a.visible) == 0:
		body = ui.EmptyState(a.theme, "reading the network…", a.width, a.tableHeight()+1)
	case len(a.visible) == 0 && a.filter != "":
		body = ui.EmptyState(a.theme, "no link matches "+strconv.Quote(a.filter),
			a.width, a.tableHeight()+1)
	case len(a.visible) == 0 && a.loadFailed:
		body = ui.EmptyState(a.theme,
			"could not read the network — see the message below",
			a.width, a.tableHeight()+1)
	case len(a.visible) == 0:
		body = ui.EmptyState(a.theme, "this machine reports no network links",
			a.width, a.tableHeight()+1)
	default:
		body = a.linksTable()
	}

	help := ui.HelpBar(a.theme, a.shortHelpKeys(), a.width)
	status := ui.StatusLine(a.theme, a.statusKind, a.status, a.defaultStatus(), a.width)
	return strings.Join([]string{header, body, help, status}, "\n")
}

// headerView renders the facts at the top of both screens.
func (a *app) headerView(subtitleExtra string) string {
	t := a.theme

	managerValue, managerStyle := "running", t.OK
	if !a.model.Running {
		managerValue, managerStyle = "not running", t.Danger
	}
	facts := []ui.Fact{{Label: "networkd", Value: managerValue, Style: &managerStyle}}

	resolved := "not running"
	if a.model.ResolvedRunning {
		resolved = "running"
	}
	facts = append(facts, ui.Fact{Label: "resolved", Value: resolved})

	if a.model.ForeignManager != "" {
		style := t.Warn
		facts = append(facts, ui.Fact{Label: "also running",
			Value: a.model.ForeignManager, Style: &style})
	}
	if len(a.model.GlobalDNS) > 0 {
		facts = append(facts, ui.Fact{Label: "global dns",
			Value: strings.Join(a.model.GlobalDNS, " ")})
	}
	// The backend version, when it was probed: quiet on a tested version,
	// coloured on one nobody has run against.
	if a.backendCompat.Backend != "" {
		facts = append(facts, ui.CompatFact(t, a.backendCompat))
	}

	subtitle := a.backend.Describe()
	if subtitleExtra != "" {
		subtitle += "  ·  " + subtitleExtra
	}
	if a.filter != "" {
		subtitle += "  ·  filter: " + a.filter
	}
	return ui.Header{Title: "tui-network", Subtitle: subtitle, Facts: facts}.
		Render(t, a.width)
}

// defaultStatus is the hint shown when there is no message to report.
func (a *app) defaultStatus() string {
	count := strconv.Itoa(len(a.visible))
	if a.filter != "" {
		return count + " of " + strconv.Itoa(len(a.model.Links)) +
			" links  ·  ? for help"
	}
	return count + " links  ·  enter for detail  ·  ? for help"
}

// linksTable renders the link list, dropping columns on narrow terminals.
func (a *app) linksTable() string {
	columns := []ui.Column{
		{Title: "LINK", Width: 12, Flex: true},
		{Title: "TYPE", Width: 8},
		{Title: "STATE", Width: 9},
		{Title: "ADDRESS", Width: 18, Flex: true},
	}
	// Progressive disclosure: extra columns only when they fit.
	showGateway := a.width >= 72
	showSetup := a.width >= 92
	if showGateway {
		columns = append(columns, ui.Column{Title: "GATEWAY", Width: 15})
	}
	if showSetup {
		columns = append(columns, ui.Column{Title: "SETUP", Width: 14})
	}

	rows := make([][]string, 0, len(a.visible))
	styles := make([]*lipgloss.Style, 0, len(a.visible))
	for _, link := range a.visible {
		row := []string{
			link.Name,
			linkType(link),
			link.Operational,
			addressCell(link),
		}
		if showGateway {
			row = append(row, link.Gateway())
		}
		if showSetup {
			row = append(row, setupCell(link))
		}
		rows = append(rows, row)
		styles = append(styles, a.linkStyle(link))
	}

	return ui.Table{
		Columns:  columns,
		Rows:     rows,
		Styles:   styles,
		Selected: a.cursor,
		Offset:   a.offset,
		Height:   a.tableHeight(),
	}.Render(a.theme, a.width)
}

// linkType renders the type column, preferring the virtual kind when there is
// one: "bridge" says more than "ether".
func linkType(l network.Link) string {
	if l.Kind != "" {
		return l.Kind
	}
	return l.Type
}

// addressCell renders the address column: the primary address, and how many
// more there are.
func addressCell(l network.Link) string {
	primary := l.PrimaryAddress()
	if primary == "" {
		return ""
	}
	if extra := len(l.Addresses) - 1; extra > 0 {
		return primary + " +" + strconv.Itoa(extra)
	}
	return primary
}

// setupCell renders the setup column, marking a link this tool will not change.
func setupCell(l network.Link) string {
	if !l.Managed {
		return l.Setup + " ·ro"
	}
	return l.Setup
}

// linkStyle colors a row by its operational state, so a link that is down
// stands out from one that is routable.
func (a *app) linkStyle(l network.Link) *lipgloss.Style {
	var style lipgloss.Style
	switch {
	case !l.Managed:
		style = a.theme.Row.Foreground(a.theme.Muted.GetForeground())
	case l.Operational == "routable":
		style = a.theme.Row.Foreground(a.theme.OK.GetForeground())
	case l.Operational == "off" || l.Operational == "no-carrier":
		style = a.theme.Row.Foreground(a.theme.Danger.GetForeground())
	case l.Operational == "degraded" || l.Setup == network.SetupConfiguring:
		style = a.theme.Row.Foreground(a.theme.Warn.GetForeground())
	default:
		style = a.theme.Row
	}
	return &style
}

// detailView renders one link in full: its facts, addresses, routes, DNS, the
// .network file that configures it and what networkd said about it.
func (a *app) detailView() string {
	header := a.headerView(a.detail.Label())
	lines := a.detailLines()

	height := a.detailHeight()
	offset := min(a.detailOffset, max(len(lines)-height, 0))
	a.detailOffset = offset
	end := min(offset+height, len(lines))

	body := make([]string, 0, height)
	for _, line := range lines[offset:end] {
		body = append(body, a.theme.Row.Width(a.width).Render(
			ui.Truncate(line, a.width-2)))
	}
	for i := len(body); i < height; i++ {
		body = append(body, a.theme.Row.Width(a.width).Render(""))
	}

	help := ui.HelpBar(a.theme, a.detailHelpKeys(), a.width)
	position := strconv.Itoa(offset+1) + "–" + strconv.Itoa(end) +
		" of " + strconv.Itoa(len(lines)) + " lines  ·  esc to go back"
	status := ui.StatusLine(a.theme, a.statusKind, a.status, position, a.width)
	return strings.Join([]string{header,
		strings.Join(body, "\n"), help, status}, "\n")
}

// detailLines builds the detail screen's text, section by section. It returns
// plain strings so the screen can be scrolled and width-truncated in one place.
func (a *app) detailLines() []string {
	link := a.detail
	lines := []string{
		"Link " + strconv.Itoa(link.Index) + ": " + link.Name,
		"",
		"  state          " + link.Operational + " (" + orNone(link.Setup) + ")",
		"  type           " + orNone(link.Type) + kindSuffix(link),
		"  online         " + orNone(link.Online),
		"  mac            " + orNone(link.MAC),
		"  mtu            " + strconv.Itoa(link.MTU),
	}
	if link.Driver != "" {
		lines = append(lines, "  driver         "+link.Driver)
	}
	if !link.Managed {
		lines = append(lines, "  read-only      "+link.ReadOnlyReason)
	}

	lines = append(lines, "", "Addresses")
	if len(link.Addresses) == 0 {
		lines = append(lines, "  (none)")
	}
	for _, address := range link.Addresses {
		suffix := ""
		if address.Source != "" {
			suffix = "  " + address.Source
		}
		if address.Provider != "" {
			suffix += " via " + address.Provider
		}
		lines = append(lines, "  "+address.String()+"  "+address.Scope+suffix)
	}

	lines = append(lines, "", "Routes")
	routes := a.model.RoutesFor(link.Name)
	if len(routes) == 0 {
		lines = append(lines, "  (none)")
	}
	for _, route := range routes {
		lines = append(lines, "  "+routeLine(route))
	}

	lines = append(lines, "", "DNS")
	lines = append(lines, "  servers        "+orNone(strings.Join(link.DNS, " ")))
	lines = append(lines, "  search domains "+
		orNone(strings.Join(link.SearchDomains, " ")))

	if link.DHCP.Enabled {
		lines = append(lines, "", "DHCP lease")
		lines = append(lines,
			"  address        "+orNone(link.DHCP.Address),
			"  server         "+orNone(link.DHCP.Server))
		if link.DHCP.ClientID != "" {
			lines = append(lines, "  client id      "+link.DHCP.ClientID)
		}
		if link.DHCP.DUID != "" {
			lines = append(lines, "  duid           "+link.DHCP.DUID)
		}
		if link.DHCP.LeaseTimestamp != "" {
			lines = append(lines,
				"  granted        "+link.DHCP.LeaseTimestamp,
				"  renew at       "+orNone(link.DHCP.Timeout1),
				"  rebind at      "+orNone(link.DHCP.Timeout2))
		}
	}

	lines = append(lines, a.configLines(link)...)

	lines = append(lines, "", "systemd-networkd journal")
	if len(a.detailJournal) == 0 {
		lines = append(lines, "  (reading…)")
	}
	for _, entry := range a.detailJournal {
		lines = append(lines, "  "+entry)
	}
	return lines
}

// configLines renders the .network file that configures a link, read-only.
func (a *app) configLines(link network.Link) []string {
	file, ok := a.model.ConfigFor(link)
	if !ok {
		return []string{"", "Network file",
			"  (none matches this link — press e to write one)"}
	}
	lines := []string{"", "Network file: " + file.Path}
	for _, dropin := range link.NetworkFileDropins {
		lines = append(lines, "  drop-in: "+dropin)
	}
	section := ""
	for _, setting := range file.Settings {
		if setting.Section != section {
			section = setting.Section
			lines = append(lines, "  ["+section+"]")
		}
		lines = append(lines, "    "+setting.Key+"="+setting.Value)
	}
	return lines
}

// routeLine renders one route as a single line.
func routeLine(r network.Route) string {
	parts := []string{r.Destination}
	if r.Gateway != "" {
		parts = append(parts, "via "+r.Gateway)
	}
	if r.Protocol != "" {
		parts = append(parts, "proto "+r.Protocol)
	}
	if r.Scope != "" {
		parts = append(parts, "scope "+r.Scope)
	}
	if r.Source != "" {
		parts = append(parts, "src "+r.Source)
	}
	if r.Metric > 0 {
		parts = append(parts, "metric "+strconv.Itoa(r.Metric))
	}
	return strings.Join(parts, " ")
}

// kindSuffix appends the virtual device kind to the type line.
func kindSuffix(l network.Link) string {
	if l.Kind == "" {
		return ""
	}
	return " (" + l.Kind + ")"
}

// orNone renders an empty value as a visible placeholder, so a blank line is
// never mistaken for a missing read.
func orNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

// dialogDiffLines is the most diff the confirm dialog will show. The kit's
// dialog does not scroll, so a diff longer than the terminal would push its
// own title and the command preview off the screen — and the command preview
// is the one thing that must never be missed.
const dialogDiffLines = 14

// diffForDialog trims a diff to what fits above the command preview, saying
// how much was left out. The whole file is on the detail screen, which is
// where a reader who wants every line should be looking anyway.
func (a *app) diffForDialog(diff string) string {
	budget := max(min(a.height-12, dialogDiffLines), 4)
	lines := strings.Split(strings.TrimSuffix(diff, "\n"), "\n")
	if len(lines) <= budget {
		return diff
	}
	kept := append([]string{}, lines[:budget]...)
	return strings.Join(kept, "\n") + "\n… " +
		strconv.Itoa(len(lines)-budget) + " more diff lines"
}

// shortHelpKeys is the single-line hint bar of the overview.
func (a *app) shortHelpKeys() []ui.KeyHint {
	hints := []ui.KeyHint{{Key: "enter", Desc: "detail"}}
	if a.caps.SupportsUpDown {
		hints = append(hints,
			ui.KeyHint{Key: "u", Desc: "up"}, ui.KeyHint{Key: "d", Desc: "down"})
	}
	return append(hints,
		ui.KeyHint{Key: "c", Desc: "reconfigure"},
		ui.KeyHint{Key: "e", Desc: "edit file"},
		ui.KeyHint{Key: "s", Desc: "dns"},
		ui.KeyHint{Key: "r", Desc: "reload"},
		ui.KeyHint{Key: "/", Desc: "filter"},
		ui.KeyHint{Key: "?", Desc: "help"},
		ui.KeyHint{Key: "q", Desc: "quit"})
}

// detailHelpKeys is the hint bar of the detail screen.
func (a *app) detailHelpKeys() []ui.KeyHint {
	hints := []ui.KeyHint{}
	if a.caps.SupportsUpDown {
		hints = append(hints,
			ui.KeyHint{Key: "u", Desc: "up"}, ui.KeyHint{Key: "d", Desc: "down"})
	}
	return append(hints,
		ui.KeyHint{Key: "c", Desc: "reconfigure"},
		ui.KeyHint{Key: "n", Desc: "renew"},
		ui.KeyHint{Key: "e", Desc: "edit file"},
		ui.KeyHint{Key: "s", Desc: "dns"},
		ui.KeyHint{Key: "S", Desc: "domains"},
		ui.KeyHint{Key: "f", Desc: "flush"},
		ui.KeyHint{Key: "esc", Desc: "back"})
}

// helpKeys is the full key list shown on the help screen.
func helpKeys() []ui.KeyHint {
	return []ui.KeyHint{
		{Key: "↑/k, ↓/j", Desc: "move the selection, or scroll the detail screen"},
		{Key: "g / G", Desc: "first / last link"},
		{Key: "pgup/pgdn", Desc: "scroll a page"},
		{Key: "enter", Desc: "open the selected link"},
		{Key: "esc", Desc: "leave the detail screen"},
		{Key: "/", Desc: "filter the links (esc clears)"},
		{Key: "u / d", Desc: "bring the link up / take it down"},
		{Key: "c", Desc: "reconfigure the link from its .network file"},
		{Key: "n", Desc: "renew the link's dynamic lease"},
		{Key: "r", Desc: "reload the networkd configuration files"},
		{Key: "f", Desc: "flush the resolver cache"},
		{Key: "s / S", Desc: "set the link's DNS servers / search domains"},
		{Key: "e", Desc: "edit the link's .network file, with a diff to confirm"},
		{Key: "R", Desc: "re-read the network"},
		{Key: "?", Desc: "this help"},
		{Key: "q", Desc: "quit"},
		{Key: "", Desc: ""},
		{Key: "note", Desc: "every change is previewed and confirmed first"},
		{Key: "note", Desc: "a link another manager owns is shown read-only"},
	}
}
