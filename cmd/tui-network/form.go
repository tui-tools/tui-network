package main

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/tui-tools/tui-kit/theme"
	"github.com/tui-tools/tui-kit/ui"
	"github.com/tui-tools/tui-network/internal/network"
)

// fieldKind tells a cycled choice from a free-text field.
type fieldKind int

const (
	fieldChoice fieldKind = iota
	fieldText
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
	// help is a one-line hint shown under the form.
	help string
}

// value returns the current value of the field.
func (f formField) value() string {
	if f.kind == fieldChoice {
		if f.choice < 0 || f.choice >= len(f.options) {
			return ""
		}
		return f.options[f.choice]
	}
	return strings.TrimSpace(f.input.Value())
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
	// link is the interface the file is being written for, kept for the title.
	link string
}

// newConfigForm builds the form, seeded from the file that already exists.
func newConfigForm(link string, spec network.FileSpec,
	caps network.Capabilities) configForm {
	text := func(placeholder, value string) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = placeholder
		ti.SetValue(value)
		ti.CharLimit = 200
		ti.Prompt = ""
		return ti
	}

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

	f := configForm{fields: fields, link: link}
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

	lines := []string{t.Title.Render("Network file for " + f.link), ""}
	for i, field := range f.fields {
		label := t.Muted.Render(ui.Pad(field.label, labelWidth))
		var value string
		switch {
		case field.kind == fieldChoice:
			value = renderChoice(t, field, i == f.active, valueWidth)
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

// renderIdleText draws a text field that does not have focus.
func renderIdleText(t theme.Theme, field formField, width int) string {
	value := field.value()
	if value == "" {
		return t.Muted.Render(ui.Truncate(field.input.Placeholder, width))
	}
	return t.Base.Render(ui.Truncate(value, width))
}
