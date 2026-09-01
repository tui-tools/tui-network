package main

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/tui-tools/tui-kit/theme"
	"github.com/tui-tools/tui-kit/ui"
)

// memberPicker is a multiple-choice list: the links a bridge gathers. The kit's
// Picker answers one question with one answer, and a bridge is the one place in
// this tool where the answer is a set, so the toggling list lives here rather
// than in the kit.
//
// It reads like the kit's picker on purpose — same keys to move, same keys to
// accept and cancel — with space as the only addition.
type memberPicker struct {
	title   string
	options []string
	// chosen[option] reports that the option is selected.
	chosen map[string]bool
	cursor int
	// done reports that the dialog finished; accepted tells submit from cancel.
	done     bool
	accepted bool
}

// newMemberPicker builds the list, with the links already chosen ticked.
func newMemberPicker(title string, options, selected []string) memberPicker {
	chosen := make(map[string]bool, len(selected))
	for _, name := range selected {
		chosen[name] = true
	}
	return memberPicker{title: title, options: options, chosen: chosen}
}

// update handles navigation, toggling and the answer.
func (p *memberPicker) update(msg tea.Msg) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return
	}
	switch key.String() {
	case "up", "k":
		p.cursor = max(p.cursor-1, 0)
	case "down", "j":
		p.cursor = min(p.cursor+1, max(len(p.options)-1, 0))
	case "home", "g":
		p.cursor = 0
	case "end", "G":
		p.cursor = max(len(p.options)-1, 0)
	case " ", "x":
		if name, ok := p.current(); ok {
			p.chosen[name] = !p.chosen[name]
		}
	case "enter":
		p.done, p.accepted = true, true
	case "esc", "q", "ctrl+c":
		p.done, p.accepted = true, false
	}
}

// current is the highlighted option.
func (p memberPicker) current() (string, bool) {
	if p.cursor < 0 || p.cursor >= len(p.options) {
		return "", false
	}
	return p.options[p.cursor], true
}

// selected returns the ticked options, in the order they are listed, so the
// rendered member lines do not shuffle between two runs of the same form.
func (p memberPicker) selected() []string {
	var out []string
	for _, name := range p.options {
		if p.chosen[name] {
			out = append(out, name)
		}
	}
	return out
}

// view renders the list centered in the given area.
func (p memberPicker) view(t theme.Theme, width, height int) string {
	lines := []string{t.Title.Render(p.title), ""}
	if len(p.options) == 0 {
		lines = append(lines, t.Muted.Render(
			"(no link here is one systemd-networkd manages)"))
	}

	longest := 0
	for _, option := range p.options {
		if w := lipgloss.Width(option); w > longest {
			longest = w
		}
	}
	for i, option := range p.options {
		mark := "[ ] "
		if p.chosen[option] {
			mark = "[x] "
		}
		label := mark + ui.Pad(option, longest)
		if i == p.cursor {
			lines = append(lines, t.SelRow.Render("> "+label))
			continue
		}
		lines = append(lines, t.Row.Render("  "+label))
	}

	lines = append(lines, "", t.Muted.Render("Chosen: "+
		orNone(strings.Join(p.selected(), " "))), "",
		t.Key.Render("↑/↓")+t.KeyDesc.Render(" move    ")+
			t.Key.Render("space")+t.KeyDesc.Render(" toggle    ")+
			t.Key.Render("enter")+t.KeyDesc.Render(" done    ")+
			t.Key.Render("esc")+t.KeyDesc.Render(" cancel"))

	box := t.Dialog.MaxWidth(max(width-4, 20)).Render(strings.Join(lines, "\n"))
	return placeCenter(box, width, height)
}
