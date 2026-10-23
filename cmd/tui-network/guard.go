package main

import (
	"fmt"
	"strconv"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/tui-tools/tui-kit/ui"
	"github.com/tui-tools/tui-network/internal/network"
)

// guardTimeout is how long a guarded change waits for the operator to keep it
// before the manager rolls it back by itself. It mirrors the "apply, then keep
// or revert" of a router's web UI, and tui-firewall's staged apply.
const guardTimeout = 60 * time.Second

// rollbackSettle is how long after the countdown ends the model is re-read,
// so the read sees the manager's own rollback rather than racing it.
const rollbackSettle = 2 * time.Second

// keepState is the rollback point a change is waiting on.
type keepState struct {
	// id is the rollback point's identifier, empty when nothing is armed.
	id string
	// title is the change's title, for the dialog and the status line.
	title string
	// deadline is when the manager rolls back on its own.
	deadline time.Time
	// failure is the error a step of the change ended with, if one did.
	failure error
}

// keepDecision is the payload of the keep-or-roll-back dialog.
type keepDecision struct {
	id string
}

// keepTickMsg drives the countdown, once a second.
type keepTickMsg struct {
	id string
}

// rollbackSettledMsg asks for a re-read once the manager's own rollback has
// had time to finish.
type rollbackSettledMsg struct{}

// setConfirm opens a confirm dialog for a plan against the links backend,
// wrapping it in the backend's automatic rollback when the backend has one
// and the plan can cut the session. The arm command becomes the plan's first
// command, so it is previewed and confirmed like the rest.
func (a *app) setConfirm(c ui.Confirm) {
	if p, ok := c.Payload.(plan); ok && a.guard != nil && !p.viaDHCP && dangerous(p.commands) {
		arm, err := a.guard.BuildArm(guardTimeout)
		if err != nil {
			c.Body += "\n" + a.wrapForDialog("No automatic rollback: "+err.Error()+".")
		} else {
			p.commands = append([]network.Command{arm}, p.commands...)
			p.guarded = true
			c.Payload = p
			c.Command = a.previewAll(p.commands, false)
			c.Body += "\n" + a.wrapForDialog(fmt.Sprintf("A checkpoint is taken first: "+
				"%s rolls this back on its own in %d s unless you keep it in the "+
				"next dialog, so a change that cuts this session undoes itself.",
				a.caps.Manager, int(guardTimeout/time.Second)))
		}
	}
	a.mode = modeConfirm
	a.confirm = c
}

// dangerous reports whether any command of a plan can drop the session.
func dangerous(commands []network.Command) bool {
	for _, cmd := range commands {
		if cmd.Destructive {
			return true
		}
	}
	return false
}

// afterGuarded handles the end of a guarded run: it reads the rollback point
// the first command armed and asks whether to keep the change.
func (a *app) afterGuarded(msg ranMsg) tea.Cmd {
	if len(msg.outputs) == 0 {
		// The arm itself failed, so nothing else ran.
		a.setStatus(ui.StatusError, "no checkpoint was taken, nothing was changed: "+
			errText(msg.err))
		return a.load()
	}
	id, err := a.guard.ArmedID(msg.outputs[0])
	if err != nil {
		a.setStatus(ui.StatusError, err.Error())
		return a.load()
	}
	a.keep = keepState{id: id, title: msg.title,
		deadline: a.now().Add(guardTimeout), failure: msg.err}
	if err := a.openKeepDialog(); err != nil {
		a.setStatus(ui.StatusError, err.Error())
		return a.load()
	}
	a.loading = true
	return tea.Batch(a.load(), a.tickKeep())
}

// errText renders an error that may be nil.
func errText(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

// openKeepDialog shows the keep-or-roll-back question with the time left.
func (a *app) openKeepDialog() error {
	keepCmd, err := a.guard.BuildKeep(a.keep.id)
	if err != nil {
		return err
	}
	rollbackCmd, err := a.guard.BuildRollback(a.keep.id)
	if err != nil {
		return err
	}
	left := int(a.keep.deadline.Sub(a.now()).Round(time.Second) / time.Second)
	body := "The change ran. Can you still reach this machine?\n" +
		"y keeps it. n rolls it back now, with:\n  " + a.backend.Preview(rollbackCmd) + "\n" +
		"With no answer, " + a.caps.Manager + " rolls it back by itself in " +
		strconv.Itoa(max(left, 0)) + " s."
	if a.keep.failure != nil {
		body = "A step of the change failed: " + a.keep.failure.Error() + "\n" +
			"Rolling back puts everything as it was before the checkpoint.\n" +
			"y keeps what did run. n rolls it back now, with:\n  " +
			a.backend.Preview(rollbackCmd) + "\n" +
			"With no answer, " + a.caps.Manager + " rolls it back by itself in " +
			strconv.Itoa(max(left, 0)) + " s."
	}
	a.mode = modeConfirm
	a.confirm = ui.Confirm{
		Title:   "Keep this change? " + a.keep.title,
		Body:    a.wrapForDialog(body),
		Command: a.backend.Preview(keepCmd),
		Danger:  true,
		Payload: keepDecision{id: a.keep.id},
	}
	return nil
}

// tickKeep schedules the next countdown tick.
func (a *app) tickKeep() tea.Cmd {
	id := a.keep.id
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return keepTickMsg{id: id} })
}

// keepTick refreshes the countdown, and closes the dialog once the manager has
// rolled the change back on its own.
func (a *app) keepTick(msg keepTickMsg) tea.Cmd {
	if a.keep.id == "" || msg.id != a.keep.id {
		return nil
	}
	if _, open := a.confirm.Payload.(keepDecision); !open || a.mode != modeConfirm {
		return nil
	}
	if !a.now().Before(a.keep.deadline) {
		title := a.keep.title
		a.keep = keepState{}
		a.confirm = ui.Confirm{}
		a.mode = a.returnMode()
		a.setStatusf(ui.StatusWarn, "%s: no answer in %d s, so %s rolled it back",
			title, int(guardTimeout/time.Second), a.caps.Manager)
		return tea.Tick(rollbackSettle, func(time.Time) tea.Msg { return rollbackSettledMsg{} })
	}
	scroll := a.confirm.Scroll
	if err := a.openKeepDialog(); err != nil {
		return nil
	}
	a.confirm.Scroll = scroll
	return a.tickKeep()
}

// decideKeep runs the operator's answer: keep the change, or roll it back now.
// Both are previewed commands like any other, run without a guard of their
// own.
func (a *app) decideKeep(decision keepDecision, keep bool) tea.Cmd {
	a.keep = keepState{}
	build, title := a.guard.BuildRollback, "Roll the change back"
	if keep {
		build, title = a.guard.BuildKeep, "Keep the change"
	}
	cmd, err := build(decision.id)
	if err != nil {
		a.setStatus(ui.StatusError, err.Error())
		return nil
	}
	a.busy = true
	a.setStatusf(ui.StatusInfo, "running %s…", a.backend.Preview(cmd))
	return a.run(plan{title: title, commands: []network.Command{cmd}})
}
