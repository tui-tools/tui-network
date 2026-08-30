package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-kit/theme"
	"github.com/tui-tools/tui-kit/ui"
	"github.com/tui-tools/tui-network/internal/network"
	"github.com/tui-tools/tui-network/internal/networkd"
)

// mode is the screen the app currently shows. Only one dialog is open at a
// time, which keeps the update loop flat.
type mode int

const (
	modeLinks mode = iota
	modeDetail
	modeConfirm
	modeFilter
	modeInput
	modePicker
	modeForm
	modeHelp
)

// inputTarget says what a text prompt's answer applies to.
type inputTarget int

const (
	inputNone inputTarget = iota
	inputDNS
	inputDomains
)

// app is the tui-network Bubble Tea model.
type app struct {
	backend network.Backend
	theme   theme.Theme
	caps    network.Capabilities
	// backendCompat is what the version probe found, rendered in the header.
	backendCompat compat.Result

	model network.Model
	// visible holds the links left after the filter, in display order.
	visible []network.Link

	width, height int
	cursor        int
	offset        int
	filter        string

	// detail holds the link the detail screen is showing, re-read in full.
	detail network.Link
	// detailJournal is what networkd said about that link.
	detailJournal []string
	// detailOffset is the detail screen's scroll position.
	detailOffset int

	mode    mode
	confirm ui.Confirm
	input   ui.Input
	picker  ui.Picker
	form    configForm

	inputFor inputTarget

	status     string
	statusKind ui.StatusKind
	loading    bool
	// loadFailed reports that the last Load returned an error, so the empty
	// state does not claim the machine simply has no links.
	loadFailed bool
	// busy blocks input while a command runs.
	busy bool
}

// loadedMsg carries the result of a Load.
type loadedMsg struct {
	model network.Model
	err   error
}

// detailMsg carries the result of a per-link read.
type detailMsg struct {
	link    network.Link
	journal []string
	err     error
}

// ranMsg carries the result of running a plan.
type ranMsg struct {
	// title is the plan's title, echoed in the status line.
	title  string
	output string
	err    error
}

// plan is what a confirm dialog is holding: one or more commands, run in
// order. Most actions are a single command; writing a .network file is two,
// the install and the reload, and both are shown before either runs.
type plan struct {
	title    string
	commands []network.Command
}

// newApp builds the model around a backend.
func newApp(backend network.Backend, th theme.Theme,
	backendCompat compat.Result) *app {
	a := &app{
		backend:       backend,
		theme:         th,
		caps:          backend.Capabilities(),
		backendCompat: backendCompat,
		width:         80,
		height:        24,
		loading:       true,
	}
	// `networkctl up` and `down` arrived in a later systemd than the rest of
	// the verbs. On an older one the keys are dropped from the help bar
	// instead of failing when pressed — and which version that is stays in
	// the manifest, not in a comparison written here.
	if !backendCompat.Caps().Has(networkd.FeatureLinkUpDown) {
		a.caps.SupportsUpDown = false
	}
	if th.Warning != "" {
		a.setStatus(ui.StatusWarn, th.Warning)
	}
	return a
}

// Init starts the first load.
func (a *app) Init() tea.Cmd { return a.load() }

// load reads the network state in the background.
func (a *app) load() tea.Cmd {
	backend := a.backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		model, err := backend.Load(ctx)
		return loadedMsg{model: model, err: err}
	}
}

// loadDetail re-reads one link and its journal in the background.
func (a *app) loadDetail(name string) tea.Cmd {
	backend := a.backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		link, err := backend.LoadLink(ctx, name)
		if err != nil {
			return detailMsg{err: err}
		}
		// The journal is a nice-to-have: a machine with no readable journal
		// still gets the rest of the screen.
		journal, journalErr := backend.Journal(ctx, name)
		if journalErr != nil {
			journal = []string{"(journal unavailable: " + journalErr.Error() + ")"}
		}
		return detailMsg{link: link, journal: journal}
	}
}

// run executes a confirmed plan in the background, one command at a time,
// stopping at the first failure.
func (a *app) run(p plan) tea.Cmd {
	backend := a.backend
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		var outputs []string
		for _, cmd := range p.commands {
			out, err := backend.Run(ctx, cmd)
			if err != nil {
				return ranMsg{title: p.title, output: out, err: err}
			}
			if trimmed := strings.TrimSpace(out); trimmed != "" {
				outputs = append(outputs, trimmed)
			}
		}
		return ranMsg{title: p.title, output: strings.Join(outputs, "; ")}
	}
}

// setStatus records a plain message for the status line.
func (a *app) setStatus(kind ui.StatusKind, message string) {
	a.status = message
	a.statusKind = kind
}

// setStatusf records a formatted message for the status line.
func (a *app) setStatusf(kind ui.StatusKind, format string, args ...any) {
	a.setStatus(kind, fmt.Sprintf(format, args...))
}

// Update is the main event loop.
func (a *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		a.clampCursor()
		return a, nil

	case loadedMsg:
		a.loading = false
		if msg.err != nil {
			a.loadFailed = true
			a.setStatus(ui.StatusError, msg.err.Error())
			return a, nil
		}
		a.loadFailed = false
		a.model = msg.model
		a.applyFilter()
		// A detail screen that is open is showing a link the reload may have
		// changed, so it is re-read too.
		if a.mode == modeDetail && a.detail.Name != "" {
			return a, a.loadDetail(a.detail.Name)
		}
		return a, nil

	case detailMsg:
		if msg.err != nil {
			a.setStatus(ui.StatusError, msg.err.Error())
			return a, nil
		}
		// The list carries what resolved and the routing table said, which
		// the per-link read does not repeat; keep it.
		if listed, ok := a.model.Link(msg.link.Name); ok {
			msg.link = mergeLink(listed, msg.link)
		}
		a.detail, a.detailJournal = msg.link, msg.journal
		return a, nil

	case ranMsg:
		a.busy = false
		if msg.err != nil {
			a.setStatus(ui.StatusError, msg.err.Error())
			return a, a.load()
		}
		summary := strings.TrimSpace(msg.output)
		if summary == "" {
			summary = "done"
		}
		a.setStatusf(ui.StatusOK, "%s: %s", msg.title, firstLine(summary))
		a.loading = true
		return a, a.load()

	case tea.KeyMsg:
		return a.handleKey(msg)
	}

	// Anything else (cursor blink, …) only concerns an open text input.
	if a.mode == modeFilter || a.mode == modeInput {
		cmd, _ := a.input.Update(msg)
		return a, cmd
	}
	if a.mode == modeForm {
		return a, a.form.updateActive(msg)
	}
	return a, nil
}

// mergeLink keeps the facts only the list read knows (the resolver's view of
// this link) when the per-link read did not report them.
func mergeLink(listed, detailed network.Link) network.Link {
	if len(detailed.DNS) == 0 {
		detailed.DNS = listed.DNS
	}
	if len(detailed.SearchDomains) == 0 {
		detailed.SearchDomains = listed.SearchDomains
	}
	if detailed.ReadOnlyReason == "" {
		detailed.ReadOnlyReason = listed.ReadOnlyReason
	}
	detailed.Managed = listed.Managed
	return detailed
}

// handleKey routes a key press to the open dialog, or to the current screen.
func (a *app) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// ctrl+c always quits, even mid-dialog.
	if msg.Type == tea.KeyCtrlC {
		return a, tea.Quit
	}
	if a.busy {
		// A command is running: swallow input rather than queueing surprises.
		return a, nil
	}

	switch a.mode {
	case modeConfirm:
		return a.handleConfirm(msg)
	case modeFilter:
		return a.handleFilter(msg)
	case modeInput:
		return a.handleInput(msg)
	case modePicker:
		return a.handlePicker(msg)
	case modeForm:
		return a.handleForm(msg)
	case modeHelp:
		a.mode = modeLinks
		return a, nil
	case modeDetail:
		return a.handleDetailKey(msg)
	default:
		return a.handleLinksKey(msg)
	}
}

// handleConfirm resolves the confirm dialog.
func (a *app) handleConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	a.confirm.Update(msg)
	if !a.confirm.Done {
		return a, nil
	}
	a.mode = a.returnMode()
	confirmed := a.confirm.Confirmed
	pending, ok := a.confirm.Payload.(plan)
	a.confirm = ui.Confirm{}
	if !confirmed || !ok {
		a.setStatus(ui.StatusInfo, "cancelled")
		return a, nil
	}
	a.busy = true
	a.setStatusf(ui.StatusInfo, "running %s…", a.backend.Preview(pending.commands[0]))
	return a, a.run(pending)
}

// returnMode is the screen a dialog goes back to.
func (a *app) returnMode() mode {
	if a.detail.Name != "" {
		return modeDetail
	}
	return modeLinks
}

// handleFilter resolves the filter prompt.
func (a *app) handleFilter(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	cmd, _ := a.input.Update(msg)
	if !a.input.Done {
		// Filter as the user types.
		a.filter = a.input.Value()
		a.applyFilter()
		return a, cmd
	}
	if a.input.Accepted {
		a.filter = a.input.Value()
	} else {
		a.filter = ""
	}
	a.applyFilter()
	a.mode = modeLinks
	return a, nil
}

// handleInput resolves the DNS and search domain prompts.
func (a *app) handleInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	cmd, _ := a.input.Update(msg)
	if !a.input.Done {
		return a, cmd
	}
	value := a.input.Value()
	accepted := a.input.Accepted
	target := a.inputFor
	link, _ := a.input.Payload.(string)
	a.input, a.inputFor = ui.Input{}, inputNone
	a.mode = a.returnMode()

	if !accepted {
		a.setStatus(ui.StatusInfo, "cancelled")
		return a, nil
	}
	values := strings.Fields(value)
	switch target {
	case inputDNS:
		return a, a.buildAndConfirm("Set DNS servers", func() (network.Command, error) {
			return a.backend.BuildSetDNS(link, values)
		})
	case inputDomains:
		return a, a.buildAndConfirm("Set search domains", func() (network.Command, error) {
			return a.backend.BuildSetDomains(link, values)
		})
	default:
		return a, nil
	}
}

// handlePicker resolves the open picker, which today only serves the form's
// choice fields.
func (a *app) handlePicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	a.picker.Update(msg)
	if !a.picker.Done {
		return a, nil
	}
	choice, accepted := a.picker.Selected(), a.picker.Accepted
	a.picker = ui.Picker{}
	if accepted {
		a.form.setActiveValue(choice)
	}
	a.mode = modeForm
	return a, nil
}

// handleForm routes keys to the .network editor.
func (a *app) handleForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		a.mode = a.returnMode()
		a.setStatus(ui.StatusInfo, "cancelled")
		return a, nil
	case "tab", "down":
		a.form.next()
		return a, nil
	case "shift+tab", "up":
		a.form.prev()
		return a, nil
	case "left":
		if a.form.activeIsChoice() {
			a.form.cycle(-1)
			return a, nil
		}
	case "right":
		if a.form.activeIsChoice() {
			a.form.cycle(1)
			return a, nil
		}
	case "enter":
		if a.form.activeIsChoice() {
			// A choice field opens a picker: better than cycling a long list.
			a.picker = ui.NewPicker(a.form.activeLabel(),
				a.form.activeOptions(), a.form.activeValue())
			a.mode = modePicker
			return a, nil
		}
		return a, a.submitForm()
	}
	return a, a.form.updateActive(msg)
}

// submitForm renders the .network file, diffs it against what is on disk and
// opens the confirm dialog with both the diff and the commands that apply it.
func (a *app) submitForm() tea.Cmd {
	write, err := a.backend.BuildWriteConfig(a.form.spec())
	if err != nil {
		a.setStatus(ui.StatusError, err.Error())
		return nil
	}
	a.mode = modeConfirm
	a.confirm = ui.Confirm{
		Title:   "Write " + write.Path,
		Body:    a.diffForDialog(write.Diff),
		Command: a.previewAll(write.Commands),
		Danger:  true,
		Payload: plan{title: "Write " + write.Path, commands: write.Commands},
	}
	return nil
}

// previewAll renders every command of a plan, one per line, each with the
// prompt the dialog puts in front of the first one.
func (a *app) previewAll(commands []network.Command) string {
	previews := make([]string, 0, len(commands))
	for _, cmd := range commands {
		previews = append(previews, a.backend.Preview(cmd))
	}
	return strings.Join(previews, "\n$ ")
}

// handleLinksKey handles the overview screen.
func (a *app) handleLinksKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc":
		return a, tea.Quit
	case "?":
		a.mode = modeHelp
	case "j", "down":
		a.moveCursor(1)
	case "k", "up":
		a.moveCursor(-1)
	case "g", "home":
		a.cursor, a.offset = 0, 0
	case "G", "end":
		a.cursor = max(len(a.visible)-1, 0)
		a.clampCursor()
	case "pgdown", "ctrl+f":
		a.moveCursor(a.tableHeight())
	case "pgup", "ctrl+b":
		a.moveCursor(-a.tableHeight())
	case "/":
		a.input = ui.NewInput("Filter links", "name, address, state…", a.filter)
		a.input.Help = "Matches any column. Empty clears the filter."
		a.mode = modeFilter
	case "enter":
		return a, a.openDetail()
	case "R", "ctrl+r":
		a.loading = true
		return a, a.load()
	default:
		return a, a.handleActionKey(msg)
	}
	return a, nil
}

// handleDetailKey handles the per-link screen. The action keys are the same
// ones the overview offers, applied to the link on screen.
func (a *app) handleDetailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "backspace", "left":
		a.detail, a.detailJournal, a.detailOffset = network.Link{}, nil, 0
		a.mode = modeLinks
		return a, nil
	case "?":
		a.mode = modeHelp
		return a, nil
	case "j", "down":
		a.detailOffset++
		return a, nil
	case "k", "up":
		a.detailOffset = max(a.detailOffset-1, 0)
		return a, nil
	case "g", "home":
		a.detailOffset = 0
		return a, nil
	case "pgdown", "ctrl+f":
		a.detailOffset += a.detailHeight()
		return a, nil
	case "pgup", "ctrl+b":
		a.detailOffset = max(a.detailOffset-a.detailHeight(), 0)
		return a, nil
	case "R", "ctrl+r":
		return a, a.loadDetail(a.detail.Name)
	default:
		return a, a.handleActionKey(msg)
	}
}

// handleActionKey handles the keys that mean the same thing on both screens.
func (a *app) handleActionKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "u":
		return a.confirmLinkAction(network.ActionUp)
	case "d":
		return a.confirmLinkAction(network.ActionDown)
	case "c":
		return a.confirmLinkAction(network.ActionReconfigure)
	case "n":
		return a.confirmLinkAction(network.ActionRenew)
	case "r":
		return a.buildAndConfirm("Reload the configuration", a.backend.BuildReload)
	case "f":
		if !a.caps.SupportsFlushCaches {
			a.setStatus(ui.StatusWarn, "this backend has no resolver cache to flush")
			return nil
		}
		return a.buildAndConfirm("Flush the resolver cache", a.backend.BuildFlushCaches)
	case "s":
		return a.promptDNS(inputDNS)
	case "S":
		return a.promptDNS(inputDomains)
	case "e":
		return a.openConfigForm()
	}
	return nil
}

// currentLink is the link the action keys apply to: the one the detail screen
// is showing, or the highlighted row of the overview.
func (a *app) currentLink() (network.Link, bool) {
	if a.mode == modeDetail && a.detail.Name != "" {
		return a.detail, true
	}
	if a.cursor < 0 || a.cursor >= len(a.visible) {
		return network.Link{}, false
	}
	return a.visible[a.cursor], true
}

// requireManagedLink returns the current link, refusing one this tool must not
// change and saying why. It is the single gate every action goes through.
func (a *app) requireManagedLink() (network.Link, bool) {
	link, ok := a.currentLink()
	if !ok {
		a.setStatus(ui.StatusWarn, "no link selected")
		return network.Link{}, false
	}
	if !link.Managed {
		reason := link.ReadOnlyReason
		if reason == "" {
			reason = "this link is not managed by systemd-networkd"
		}
		a.setStatusf(ui.StatusWarn, "%s is read-only: %s", link.Name, reason)
		return network.Link{}, false
	}
	return link, true
}

// openDetail re-reads the highlighted link and opens its screen.
func (a *app) openDetail() tea.Cmd {
	link, ok := a.currentLink()
	if !ok {
		a.setStatus(ui.StatusWarn, "no link selected")
		return nil
	}
	a.detail, a.detailJournal, a.detailOffset = link, nil, 0
	a.mode = modeDetail
	return a.loadDetail(link.Name)
}

// confirmLinkAction asks before running one of the link verbs.
func (a *app) confirmLinkAction(action string) tea.Cmd {
	if (action == network.ActionUp || action == network.ActionDown) &&
		!a.caps.SupportsUpDown {
		a.setStatusf(ui.StatusWarn,
			"this systemd has no `networkctl %s`; use reconfigure instead", action)
		return nil
	}
	link, ok := a.requireManagedLink()
	if !ok {
		return nil
	}
	cmd, err := a.backend.BuildLinkAction(action, link.Name)
	if err != nil {
		a.setStatus(ui.StatusError, err.Error())
		return nil
	}
	body := cmd.Description + "."
	if action == network.ActionDown || action == network.ActionReconfigure {
		body += "\nIf you are connected over this link, you will lose the session."
	}
	a.openConfirm(cmd.Description, body, cmd)
	return nil
}

// promptDNS opens the prompt for a link's DNS servers or search domains.
func (a *app) promptDNS(target inputTarget) tea.Cmd {
	if !a.caps.SupportsRuntimeDNS {
		a.setStatus(ui.StatusWarn, "this backend cannot set DNS at runtime")
		return nil
	}
	link, ok := a.requireManagedLink()
	if !ok {
		return nil
	}
	title, placeholder, help, current := "DNS servers for "+link.Name,
		"192.0.2.53 2001:db8::53",
		"Space separated. Empty clears them. Runtime only: reconfiguring the "+
			"link restores what its .network file says.",
		strings.Join(link.DNS, " ")
	if target == inputDomains {
		title, placeholder, current = "Search domains for "+link.Name,
			"example.test ~corp.test", strings.Join(link.SearchDomains, " ")
		help = "Space separated. Empty clears them. Runtime only."
	}
	a.input = ui.NewInput(title, placeholder, current)
	a.input.Help = help
	a.input.Payload = link.Name
	a.inputFor = target
	a.mode = modeInput
	return nil
}

// openConfigForm opens the guided editor for the current link's .network file.
func (a *app) openConfigForm() tea.Cmd {
	if !a.caps.SupportsFileEdit {
		a.setStatus(ui.StatusWarn, "this backend has no configuration files to edit")
		return nil
	}
	link, ok := a.requireManagedLink()
	if !ok {
		return nil
	}
	file, _ := a.model.ConfigFor(link)
	a.form = newConfigForm(link.Name, networkd.SpecFromFile(file, link.Name), a.caps)
	a.mode = modeForm
	return nil
}

// buildAndConfirm runs a command builder and opens the confirm dialog, or
// reports the builder's error in the status line.
func (a *app) buildAndConfirm(title string,
	build func() (network.Command, error)) tea.Cmd {
	cmd, err := build()
	if err != nil {
		a.setStatus(ui.StatusError, err.Error())
		return nil
	}
	a.openConfirm(title, cmd.Description+".", cmd)
	return nil
}

// openConfirm shows one command and what it does.
func (a *app) openConfirm(title, body string, cmd network.Command) {
	a.mode = modeConfirm
	a.confirm = ui.Confirm{
		Title:   title,
		Body:    body,
		Command: a.backend.Preview(cmd),
		Danger:  cmd.Destructive,
		Payload: plan{title: title, commands: []network.Command{cmd}},
	}
}

// applyFilter recomputes the visible links from the current filter.
func (a *app) applyFilter() {
	if a.filter == "" {
		a.visible = a.model.Links
		a.clampCursor()
		return
	}
	needle := strings.ToLower(a.filter)
	var kept []network.Link
	for _, link := range a.model.Links {
		if strings.Contains(strings.ToLower(linkHaystack(link)), needle) {
			kept = append(kept, link)
		}
	}
	a.visible = kept
	a.clampCursor()
}

// linkHaystack is the text the filter matches against.
func linkHaystack(l network.Link) string {
	parts := []string{
		l.Name, l.Type, l.Kind, l.Setup, l.Operational, l.MAC, l.Driver,
		l.NetworkFile, strings.Join(l.DNS, " "), strings.Join(l.Gateways, " "),
	}
	for _, address := range l.Addresses {
		parts = append(parts, address.String())
	}
	return strings.Join(parts, " ")
}

// moveCursor moves the selection and keeps the viewport in sync.
func (a *app) moveCursor(delta int) {
	a.cursor += delta
	a.clampCursor()
}

// clampCursor keeps the cursor and the scroll offset within range.
func (a *app) clampCursor() {
	if len(a.visible) == 0 {
		a.cursor, a.offset = 0, 0
		return
	}
	a.cursor = min(max(a.cursor, 0), len(a.visible)-1)

	height := a.tableHeight()
	if a.cursor < a.offset {
		a.offset = a.cursor
	}
	if a.cursor >= a.offset+height {
		a.offset = a.cursor - height + 1
	}
	a.offset = max(min(a.offset, max(len(a.visible)-height, 0)), 0)
}

// firstLine keeps status messages to one line.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
