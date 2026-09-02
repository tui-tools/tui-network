package main

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/tui-tools/tui-kit/theme"
	"github.com/tui-tools/tui-kit/ui"
	"github.com/tui-tools/tui-network/internal/dhcp"
	"github.com/tui-tools/tui-network/internal/network"
)

// formKind says what a submitted form builds: a .network file, or the DHCP
// options drop-in. One form type serves both, so the field mechanics — focus,
// cycling, the dialog rendering — live in one place.
type formKind int

const (
	formNetworkFile formKind = iota
	formDHCPOptions
	// formVLAN and formBridge build a .netdev unit and the member lines that
	// attach it, which is one plan over several files.
	formVLAN
	formBridge
)

// fieldKind tells a cycled choice from a free-text field.
type fieldKind int

const (
	fieldChoice fieldKind = iota
	fieldText
	// fieldMembers is a set rather than a value: the links a bridge gathers,
	// ticked in a list of their own.
	fieldMembers
)

// formField is one row of the .network editor.
type formField struct {
	// key identifies the field when building the FileSpec.
	key   string
	label string
	kind  fieldKind
	// options and choice hold the state of a choice field.
	options []string
	choice  int
	// input holds the state of a text field.
	input textinput.Model
	// chosen holds the state of a members field: the ticked options, in the
	// order they are listed.
	chosen []string
	// help is a one-line hint shown under the form.
	help string
}

// value returns the current value of the field, as one line.
func (f formField) value() string {
	switch f.kind {
	case fieldChoice:
		if f.choice < 0 || f.choice >= len(f.options) {
			return ""
		}
		return f.options[f.choice]
	case fieldMembers:
		return strings.Join(f.chosen, " ")
	default:
		return strings.TrimSpace(f.input.Value())
	}
}

// configForm is the guided editor for a link's .network file.
//
// It is guided rather than free: the file it writes is generated from these
// fields, so what the confirm dialog diffs is a file this tool can read back.
// The whole file stays visible in the detail view for anything the form does
// not cover.
type configForm struct {
	fields []formField
	active int
	// kind says which builder a submit goes to.
	kind formKind
	// title heads the dialog.
	title string
}

// newFormText builds one text input for a form field.
func newFormText(placeholder, value string) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.SetValue(value)
	ti.CharLimit = 200
	ti.Prompt = ""
	return ti
}

// newConfigForm builds the form, seeded from the file that already exists.
func newConfigForm(link string, spec network.FileSpec,
	caps network.Capabilities) configForm {
	text := newFormText

	dhcp := formField{key: "dhcp", label: "DHCP", kind: fieldChoice,
		options: caps.DHCPModes,
		help:    "yes covers both families; no makes the link static."}
	for i, mode := range caps.DHCPModes {
		if mode == spec.DHCP {
			dhcp.choice = i
		}
	}

	fields := []formField{
		{key: "match", label: "Match Name", kind: fieldText,
			input: text("enp1s0", spec.MatchName),
			help:  "The [Match] Name= this file applies to."},
		dhcp,
		{key: "address", label: "Address", kind: fieldText,
			input: text("192.0.2.10/24", spec.Address),
			help:  "Static address in CIDR form. Required when DHCP is no."},
		{key: "gateway", label: "Gateway", kind: fieldText,
			input: text("192.0.2.1", spec.Gateway),
			help:  "Static default gateway; empty leaves it to DHCP."},
		{key: "dns", label: "DNS", kind: fieldText,
			input: text("192.0.2.53 2001:db8::53", strings.Join(spec.DNS, " ")),
			help:  "One or more servers, separated by spaces."},
		{key: "domains", label: "Domains", kind: fieldText,
			input: text("example.test", strings.Join(spec.Domains, " ")),
			help:  "Search domains, separated by spaces."},
		{key: "path", label: "File", kind: fieldText,
			input: text(caps.ConfigDir+"/50-link.network", spec.Path),
			help:  "Where it is installed. It must be under " + caps.ConfigDir + "."},
	}

	f := configForm{fields: fields, kind: formNetworkFile,
		title: "Network file for " + link}
	f.focusActive()
	return f
}

// newDHCPOptionsForm builds the editor for the DHCP options drop-in, seeded
// from what that file — and only that file — says today. The rendered file is
// regenerated wholesale from these fields, so seeding from anywhere else would
// copy the administrator's own lines into a file the tool owns.
func newDHCPOptionsForm(opts dhcp.Options, ownFile string) configForm {
	fields := []formField{
		{key: "adns", label: "DNS servers", kind: fieldText,
			input: newFormText("192.0.2.1, 9.9.9.9", strings.Join(opts.DNS, ", ")),
			help: "Advertised to clients (option 6), comma separated. " +
				"Empty advertises this router itself, dnsmasq's default."},
		{key: "agw", label: "Gateway", kind: fieldText,
			input: newFormText("192.0.2.1", opts.Gateway),
			help: "Advertised router (option 3). " +
				"Empty advertises this router itself, dnsmasq's default."},
		{key: "domain", label: "Domain", kind: fieldText,
			input: newFormText("lan.example.test", opts.Domain),
			help:  "domain= for DHCP and local DNS names. Empty leaves it unset."},
		{key: "upstream", label: "Upstream DNS", kind: fieldText,
			input: newFormText("198.51.100.53, 9.9.9.9",
				strings.Join(opts.Upstreams, ", ")),
			help: "server= forwarders dnsmasq resolves through, comma " +
				"separated. Empty falls back to /etc/resolv.conf."},
	}
	f := configForm{fields: fields, kind: formDHCPOptions,
		title: "DHCP and DNS options — " + ownFile}
	f.focusActive()
	return f
}

// newNetworkdDHCPOptionsForm builds the editor for the [DHCPServer] drop-in.
// It is the networkd counterpart of newDHCPOptionsForm, and the fields differ
// because the servers differ: systemd-networkd's own server carries the lease
// time here rather than on the pool line, emits NTP servers of its own, and
// forwards no DNS at all — systemd-resolved does that on a networkd router, so
// there is no upstream field.
//
// It is seeded from the options in effect across the unit and its drop-ins,
// not from the drop-in alone: the drop-in clears each advertised list before
// setting it, so it has to open on what a client is handed today or an
// unchanged submit would take it away.
func newNetworkdDHCPOptionsForm(opts dhcp.Options, ownFile string) configForm {
	fields := []formField{
		{key: "adns", label: "DNS servers", kind: fieldText,
			input: newFormText("_server_address, 9.9.9.9", strings.Join(opts.DNS, ", ")),
			help: "Advertised to clients (option 6), comma separated. " +
				"_server_address is this router itself. Empty lets networkd " +
				"propagate the uplink's servers."},
		{key: "agw", label: "Gateway", kind: fieldText,
			input: newFormText("10.55.0.1", opts.Gateway),
			help: "Router= (option 3). " +
				"Empty advertises the server's own address, networkd's default."},
		{key: "ntp", label: "NTP servers", kind: fieldText,
			input: newFormText("10.55.0.1", strings.Join(opts.NTP, ", ")),
			help: "NTP= (option 42), comma separated. " +
				"Empty lets networkd propagate the uplink's servers."},
		{key: "domain", label: "Domain", kind: fieldText,
			input: newFormText("lan.example.test", opts.Domain),
			help: "The domain handed to clients. networkd has no key for it, " +
				"so it is sent as SendOption=15. Empty sends none."},
		{key: "lease", label: "Lease time", kind: fieldText,
			input: newFormText("1h", opts.LeaseTime),
			help: "DefaultLeaseTimeSec=. A count of seconds or a span such " +
				"as 30min or 12h. Empty leaves systemd's default of 1h."},
	}
	f := configForm{fields: fields, kind: formDHCPOptions,
		title: "DHCP server options — " + ownFile}
	f.focusActive()
	return f
}

// newVLANForm builds the editor for a new VLAN: the device's name, the link it
// rides on, and the 802.1Q id. The parent is a choice over the links networkd
// manages, because a VLAN on a link this tool may not change is a device that
// would never come up.
func newVLANForm(parents []string) configForm {
	fields := []formField{
		{key: "name", label: "Name", kind: fieldText,
			input: newFormText("vlan10", ""),
			help: "The device's name, and the name of the " +
				"/etc/systemd/network unit written for it."},
		{key: "parent", label: "Parent link", kind: fieldChoice, options: parents,
			help: "The link the VLAN rides on. Its .network file gains a VLAN= line."},
		{key: "id", label: "VLAN id", kind: fieldText,
			input: newFormText("10", ""),
			help:  "The 802.1Q id, between 1 and 4094."},
	}
	f := configForm{fields: fields, kind: formVLAN, title: "New VLAN"}
	f.focusActive()
	return f
}

// newBridgeForm builds the editor for a new bridge: the device's name and the
// links it gathers, ticked in a list.
func newBridgeForm(members []string) configForm {
	fields := []formField{
		{key: "name", label: "Name", kind: fieldText,
			input: newFormText("br0", ""),
			help: "The bridge's name, and the name of the " +
				"/etc/systemd/network unit written for it."},
		{key: "members", label: "Members", kind: fieldMembers, options: members,
			help: "The links to enslave; each one's .network file gains a " +
				"Bridge= line. enter opens the list, space ticks a link."},
	}
	f := configForm{fields: fields, kind: formBridge, title: "New bridge"}
	f.focusActive()
	return f
}

// focusActive moves the text cursor to the active field.
func (f *configForm) focusActive() {
	for i := range f.fields {
		if f.fields[i].kind != fieldText {
			continue
		}
		if i == f.active {
			f.fields[i].input.Focus()
			continue
		}
		f.fields[i].input.Blur()
	}
}

// next moves to the following field.
func (f *configForm) next() {
	f.active = (f.active + 1) % len(f.fields)
	f.focusActive()
}

// prev moves to the previous field.
func (f *configForm) prev() {
	f.active = (f.active - 1 + len(f.fields)) % len(f.fields)
	f.focusActive()
}

// activeIsChoice reports whether the active field is a cycled choice.
func (f configForm) activeIsChoice() bool {
	return f.fields[f.active].kind == fieldChoice
}

// activeIsMembers reports whether the active field is a ticked set.
func (f configForm) activeIsMembers() bool {
	return f.fields[f.active].kind == fieldMembers
}

// activeChosen exposes the active members field's current set to its dialog.
func (f configForm) activeChosen() []string { return f.fields[f.active].chosen }

// setActiveChosen applies the set ticked in the members dialog.
func (f *configForm) setActiveChosen(members []string) {
	f.fields[f.active].chosen = members
}

// chosen returns the ticked set of a members field, by key.
func (f configForm) chosen(key string) []string {
	for _, field := range f.fields {
		if field.key == key {
			return field.chosen
		}
	}
	return nil
}

// activeLabel, activeOptions and activeValue expose the active field to the
// picker dialog.
func (f configForm) activeLabel() string     { return f.fields[f.active].label }
func (f configForm) activeOptions() []string { return f.fields[f.active].options }
func (f configForm) activeValue() string     { return f.fields[f.active].value() }

// setActiveValue applies a value chosen in the picker.
func (f *configForm) setActiveValue(value string) {
	field := &f.fields[f.active]
	for i, o := range field.options {
		if o == value {
			field.choice = i
			return
		}
	}
}

// cycle moves a choice field one step.
func (f *configForm) cycle(delta int) {
	field := &f.fields[f.active]
	if len(field.options) == 0 {
		return
	}
	field.choice = (field.choice + delta + len(field.options)) % len(field.options)
}

// updateActive forwards a message to the active text field.
func (f *configForm) updateActive(msg tea.Msg) tea.Cmd {
	if f.fields[f.active].kind != fieldText {
		return nil
	}
	var cmd tea.Cmd
	f.fields[f.active].input, cmd = f.fields[f.active].input.Update(msg)
	return cmd
}

// get returns the value of a field by key.
func (f configForm) get(key string) string {
	for _, field := range f.fields {
		if field.key == key {
			return field.value()
		}
	}
	return ""
}

// spec turns the form into a FileSpec. Validation lives in the backend, which
// is the same code path the file renderer uses, so the form cannot approve
// something the renderer would refuse.
func (f configForm) spec() network.FileSpec {
	return network.FileSpec{
		Path:      f.get("path"),
		MatchName: f.get("match"),
		DHCP:      f.get("dhcp"),
		Address:   f.get("address"),
		Gateway:   f.get("gateway"),
		DNS:       strings.Fields(f.get("dns")),
		Domains:   strings.Fields(f.get("domains")),
	}
}

// view renders the form as a dialog.
func (f configForm) view(t theme.Theme, width, height int) string {
	labelWidth := 0
	for _, field := range f.fields {
		if w := len(field.label); w > labelWidth {
			labelWidth = w
		}
	}

	inner := min(max(width-8, 30), 72)
	valueWidth := max(inner-labelWidth-6, 10)

	lines := []string{t.Title.Render(f.title), ""}
	for i, field := range f.fields {
		label := t.Muted.Render(ui.Pad(field.label, labelWidth))
		var value string
		switch {
		case field.kind == fieldChoice:
			value = renderChoice(t, field, i == f.active, valueWidth)
		case field.kind == fieldMembers:
			value = renderMembers(t, field, valueWidth)
		case i == f.active:
			field.input.Width = valueWidth - 2
			value = field.input.View()
		default:
			value = renderIdleText(t, field, valueWidth)
		}
		marker := "  "
		if i == f.active {
			marker = t.Accent.Render("> ")
		}
		lines = append(lines, marker+label+"  "+value)
	}

	if help := f.fields[f.active].help; help != "" {
		lines = append(lines, "", t.Muted.Render(help))
	}
	lines = append(lines, "",
		t.Key.Render("tab")+t.KeyDesc.Render(" next    ")+
			t.Key.Render("←/→")+t.KeyDesc.Render(" change    ")+
			t.Key.Render("enter")+t.KeyDesc.Render(" pick/review    ")+
			t.Key.Render("esc")+t.KeyDesc.Render(" cancel"))

	box := t.Dialog.Width(inner).Render(strings.Join(lines, "\n"))
	return placeCenter(box, width, height)
}

// renderChoice draws a choice field with its cycling arrows.
func renderChoice(t theme.Theme, field formField, active bool, width int) string {
	value := ui.Truncate(field.value(), width-4)
	if active {
		return t.Accent.Render("‹ ") + t.Base.Render(value) + t.Accent.Render(" ›")
	}
	return t.Base.Render("  " + value)
}

// renderMembers draws a set field: the ticked links, or an invitation to pick
// some when none are ticked yet.
func renderMembers(t theme.Theme, field formField, width int) string {
	value := field.value()
	if value == "" {
		return t.Muted.Render(ui.Truncate("(none — enter to choose)", width))
	}
	return t.Base.Render(ui.Truncate(value, width))
}

// renderIdleText draws a text field that does not have focus.
func renderIdleText(t theme.Theme, field formField, width int) string {
	value := field.value()
	if value == "" {
		return t.Muted.Render(ui.Truncate(field.input.Placeholder, width))
	}
	return t.Base.Render(ui.Truncate(value, width))
}
