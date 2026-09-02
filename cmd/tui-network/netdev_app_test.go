package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/tui-tools/tui-network/internal/network"
	"github.com/tui-tools/tui-network/internal/networkd"
)

// setField types a value into the form field with the given key.
func setField(t *testing.T, a *app, key, value string) {
	t.Helper()
	for i := range a.form.fields {
		if a.form.fields[i].key == key {
			a.form.fields[i].input.SetValue(value)
			return
		}
	}
	t.Fatalf("the open form has no field named %q", key)
}

// focusField moves the form's cursor onto a field by key, so the next enter
// submits from a text field rather than opening that field's own dialog.
func focusField(t *testing.T, a *app, key string) {
	t.Helper()
	for i := range a.form.fields {
		if a.form.fields[i].key == key {
			a.form.active = i
			a.form.focusActive()
			return
		}
	}
	t.Fatalf("the open form has no field named %q", key)
}

// openNetdevPicker presses V and answers the kind picker.
func openNetdevPicker(t *testing.T, a *app, kind string) {
	t.Helper()
	drain(t, a, press(a, "V"))
	if a.mode != modePicker {
		t.Fatalf("V did not open the kind picker (status: %s)", a.status)
	}
	for a.picker.Selected() != kind {
		if a.picker.Cursor >= len(a.picker.Options)-1 {
			t.Fatalf("the kind picker does not offer %q", kind)
		}
		// Since tui-kit v0.3.0 the picker filters on printable keys, so the
		// cursor moves with the arrows rather than with j/k.
		drain(t, a, press(a, "down"))
	}
	drain(t, a, press(a, "enter"))
	if a.mode != modeForm {
		t.Fatalf("choosing %q did not open a form (status: %s)", kind, a.status)
	}
}

// TestCreateVLANPreviewsOneDiffAndThreeCommands is the feature's promise on the
// screen: one diff covering the unit and the parent, then the exact commands.
func TestCreateVLANPreviewsOneDiffAndThreeCommands(t *testing.T) {
	a, backend := newTestApp(t)
	openNetdevPicker(t, a, network.NetdevVLAN)

	setField(t, a, "name", "vlan10")
	setField(t, a, "id", "10")
	if got := a.form.get("parent"); got != "enp1s0" {
		t.Fatalf("parent = %q, want the sample machine's managed link", got)
	}
	focusField(t, a, "id")
	drain(t, a, press(a, "enter"))

	if a.mode != modeConfirm {
		t.Fatalf("the VLAN form did not open a confirm dialog (status: %s)", a.status)
	}
	// One dialog, both files, and the lockout warning. The warning is folded to
	// the terminal, so it is matched on its words rather than on its line
	// breaks.
	for _, want := range []string{"+Kind=vlan", "+Id=10", "+VLAN=vlan10"} {
		if !strings.Contains(a.confirm.Body, want) {
			t.Errorf("the confirm body is missing %q:\n%s", want, a.confirm.Body)
		}
	}
	for _, want := range []string{"re-parents enp1s0", "you will lose the session"} {
		if !strings.Contains(unwrapped(a.confirm.Body), want) {
			t.Errorf("the confirm body is missing %q:\n%s", want, a.confirm.Body)
		}
	}
	if !a.confirm.Danger {
		t.Errorf("creating a device is a dangerous change")
	}

	lines := strings.Split(a.confirm.Command, "\n")
	if len(lines) != 3 {
		t.Fatalf("previewed %d command lines, want two installs and a reload:\n%s",
			len(lines), a.confirm.Command)
	}
	if !strings.Contains(lines[0], "install -m 644") ||
		!strings.Contains(lines[0], "20-vlan10.netdev") {
		t.Errorf("first command = %q, want the unit's install", lines[0])
	}
	if !strings.Contains(lines[1], "10-wired.network") {
		t.Errorf("second command = %q, want the parent's install", lines[1])
	}
	if !strings.Contains(lines[2], "networkctl reload") {
		t.Errorf("last command = %q, want the reload", lines[2])
	}

	drain(t, a, press(a, "y"))
	ran := backend.Ran()
	if len(ran) != 3 {
		t.Fatalf("ran %d commands, want the ones previewed", len(ran))
	}
	for i, cmd := range ran {
		// The dialog joins the lines after the first with the kit's "$ "
		// continuation marker; what follows it is the command line itself.
		want := strings.TrimPrefix(lines[i], "$ ")
		if got := backend.Preview(cmd); got != want {
			t.Errorf("ran %q, want the previewed %q", got, want)
		}
	}
	// The demo really gained the device.
	if _, ok := a.model.Netdev("vlan10"); !ok {
		t.Errorf("the sample machine has no vlan10 unit after the write")
	}
	if _, ok := a.model.Link("vlan10"); !ok {
		t.Errorf("the sample machine shows no vlan10 link after the write")
	}
}

// TestCreateBridgeTicksItsMembers covers the multi-select half.
func TestCreateBridgeTicksItsMembers(t *testing.T) {
	a, backend := newTestApp(t)
	openNetdevPicker(t, a, network.NetdevBridge)
	setField(t, a, "name", "br0")

	focusField(t, a, "members")
	drain(t, a, press(a, "enter"))
	if a.mode != modeMembers {
		t.Fatalf("enter on the members field did not open the list")
	}
	drain(t, a, press(a, " ")) // tick the highlighted link
	drain(t, a, press(a, "enter"))
	if a.mode != modeForm {
		t.Fatalf("the member list did not return to the form")
	}
	if got := a.form.chosen("members"); len(got) != 1 || got[0] != "enp1s0" {
		t.Fatalf("members = %v, want the sample machine's managed link", got)
	}

	focusField(t, a, "name")
	drain(t, a, press(a, "enter"))
	if a.mode != modeConfirm {
		t.Fatalf("the bridge form did not open a confirm dialog (status: %s)", a.status)
	}
	for _, want := range []string{"+Kind=bridge", "+Bridge=br0"} {
		if !strings.Contains(a.confirm.Body, want) {
			t.Errorf("the confirm body is missing %q:\n%s", want, a.confirm.Body)
		}
	}
	if !strings.Contains(unwrapped(a.confirm.Body), "re-parents enp1s0") {
		t.Errorf("the confirm body carries no lockout warning:\n%s", a.confirm.Body)
	}
	drain(t, a, press(a, "y"))
	if len(backend.Ran()) != 3 {
		t.Errorf("ran %d commands, want two installs and a reload", len(backend.Ran()))
	}
	if unit, ok := a.model.Netdev("br0"); !ok || unit.Kind != network.NetdevBridge {
		t.Errorf("the sample machine has no br0 bridge after the write")
	}
}

// TestBridgeWithNoMembersIsRefused: an empty bridge is a device nothing reaches,
// and the refusal comes from the backend, which is the same code path the write
// goes through.
func TestBridgeWithNoMembersIsRefused(t *testing.T) {
	a, backend := newTestApp(t)
	openNetdevPicker(t, a, network.NetdevBridge)
	setField(t, a, "name", "br0")
	focusField(t, a, "name")
	drain(t, a, press(a, "enter"))

	if a.mode == modeConfirm {
		t.Errorf("a bridge with no members opened a confirm dialog")
	}
	if len(backend.Ran()) != 0 {
		t.Errorf("a refused bridge ran a command")
	}
	if !strings.Contains(a.status, "member") {
		t.Errorf("status = %q, want the reason", a.status)
	}
}

// TestVLANIDOutOfRangeIsRefusedInTheForm: the form parses the id with the same
// bounds the renderer enforces, so an out-of-range id never reaches a plan.
func TestVLANIDOutOfRangeIsRefusedInTheForm(t *testing.T) {
	for _, id := range []string{"0", "4095", "", "ten"} {
		a, backend := newTestApp(t)
		openNetdevPicker(t, a, network.NetdevVLAN)
		setField(t, a, "name", "vlan10")
		setField(t, a, "id", id)
		focusField(t, a, "id")
		drain(t, a, press(a, "enter"))

		if a.mode == modeConfirm {
			t.Errorf("VLAN id %q opened a confirm dialog", id)
		}
		if len(backend.Ran()) != 0 {
			t.Errorf("VLAN id %q ran a command", id)
		}
	}
}

// TestCreatingTheSameDeviceTwiceIsRefused is the collision guard from the
// screen: the second attempt finds the name taken and never builds a plan.
func TestCreatingTheSameDeviceTwiceIsRefused(t *testing.T) {
	a, backend := newTestApp(t)
	openNetdevPicker(t, a, network.NetdevVLAN)
	setField(t, a, "name", "vlan10")
	setField(t, a, "id", "10")
	focusField(t, a, "id")
	drain(t, a, press(a, "enter"))
	drain(t, a, press(a, "y"))
	if len(backend.Ran()) != 3 {
		t.Fatalf("the first VLAN ran %d commands", len(backend.Ran()))
	}

	openNetdevPicker(t, a, network.NetdevVLAN)
	setField(t, a, "name", "vlan10")
	setField(t, a, "id", "20")
	focusField(t, a, "id")
	drain(t, a, press(a, "enter"))

	if a.mode == modeConfirm {
		t.Errorf("a colliding name opened a confirm dialog")
	}
	if len(backend.Ran()) != 3 {
		t.Errorf("a refused creation ran a command")
	}
	if !strings.Contains(a.status, "already") {
		t.Errorf("status = %q, want the collision reason", a.status)
	}
}

// TestRemoveOwnedNetdevMirrorsTheCreation: X on a device tui-network created
// previews the unit's removal and the member line coming back out, then applies
// exactly that.
func TestRemoveOwnedNetdevMirrorsTheCreation(t *testing.T) {
	a, backend := newTestApp(t)
	openNetdevPicker(t, a, network.NetdevBridge)
	setField(t, a, "name", "br0")
	focusField(t, a, "members")
	drain(t, a, press(a, "enter"))
	drain(t, a, press(a, " "))
	drain(t, a, press(a, "enter"))
	focusField(t, a, "name")
	drain(t, a, press(a, "enter"))
	drain(t, a, press(a, "y"))
	if _, ok := a.model.Link("br0"); !ok {
		t.Fatalf("the bridge was not created (status: %s)", a.status)
	}

	selectLink(t, a, "br0")
	drain(t, a, press(a, "X"))
	if a.mode != modeConfirm {
		t.Fatalf("X did not open a confirm dialog (status: %s)", a.status)
	}
	for _, want := range []string{"-Kind=bridge", "-Bridge=br0", "+++ /dev/null"} {
		if !strings.Contains(a.confirm.Body, want) {
			t.Errorf("the removal body is missing %q:\n%s", want, a.confirm.Body)
		}
	}
	if !strings.Contains(unwrapped(a.confirm.Body), "you will lose the session") {
		t.Errorf("the removal body carries no lockout warning:\n%s", a.confirm.Body)
	}
	lines := strings.Split(a.confirm.Command, "\n")
	if len(lines) != 3 {
		t.Fatalf("previewed %d command lines, want the rm, the install and the reload:\n%s",
			len(lines), a.confirm.Command)
	}
	if !strings.Contains(lines[0], "rm -f -- ") ||
		!strings.Contains(lines[0], "20-br0.netdev") {
		t.Errorf("first command = %q, want the unit's removal", lines[0])
	}

	before := len(backend.Ran())
	drain(t, a, press(a, "y"))
	if got := len(backend.Ran()) - before; got != 3 {
		t.Fatalf("removing ran %d commands, want 3", got)
	}
	if _, ok := a.model.Netdev("br0"); ok {
		t.Errorf("the unit survived its removal")
	}
	if _, ok := a.model.Link("br0"); ok {
		t.Errorf("the device survived its removal")
	}
}

// TestRemoveRefusesALinkTheToolDoesNotOwn: a physical link, and a unit somebody
// else wrote, are both refused with the reason in the status line.
func TestRemoveRefusesALinkTheToolDoesNotOwn(t *testing.T) {
	a, backend := newTestApp(t)
	selectLink(t, a, "enp1s0")
	drain(t, a, press(a, "X"))
	if a.mode == modeConfirm {
		t.Errorf("X opened a dialog for a physical link")
	}
	if len(backend.Ran()) != 0 {
		t.Errorf("a refused removal ran a command")
	}
	if !strings.Contains(a.status, "no .netdev unit") {
		t.Errorf("status = %q, want the reason", a.status)
	}

	// A unit somebody else wrote is theirs: the tool shows it and leaves it.
	a.model.NetdevFiles = append(a.model.NetdevFiles,
		networkd.ParseNetdevFile("/etc/systemd/network/20-br9.netdev",
			"[NetDev]\nName=br9\nKind=bridge\n"))
	a.model.Links = append(a.model.Links, network.Link{
		Name: "br9", Type: "ether", Kind: "bridge", Managed: true})
	a.applyFilter()
	selectLink(t, a, "br9")
	drain(t, a, press(a, "X"))
	if a.mode == modeConfirm {
		t.Errorf("X opened a dialog for a unit tui-network did not write")
	}
	if !strings.Contains(a.status, "not written by tui-network") {
		t.Errorf("status = %q, want the ownership reason", a.status)
	}
}

// TestNetdevDialogsRenderAtEveryWidth keeps the two new dialogs inside the
// responsive contract the rest of the screens are held to.
func TestNetdevDialogsRenderAtEveryWidth(t *testing.T) {
	for width := 40; width <= 200; width += 8 {
		a, _ := newTestApp(t)
		a.width, a.height = width, 24
		a.clampCursor()

		screens := map[string]func(){
			"vlan form": func() {
				a.mode = modeForm
				a.form = newVLANForm(a.managedLinkNames())
			},
			"bridge form": func() {
				a.mode = modeForm
				a.form = newBridgeForm(a.managedLinkNames())
			},
			"members": func() {
				a.mode = modeMembers
				a.members = newMemberPicker("Members",
					a.managedLinkNames(), []string{"enp1s0"})
			},
		}
		for name, setup := range screens {
			setup()
			for i, line := range strings.Split(a.View(), "\n") {
				if got := lineWidth(line); got > width {
					t.Fatalf("%s at %d cols: line %d is %d cells wide",
						name, width, i, got)
				}
			}
		}
	}
}

// TestDiffForDialogKeepsEveryFileVisible is the multi-file dialog rule: when a
// plan's diff is too long for the dialog, no file falls off the bottom — each
// one keeps a share and says how much of it was left out.
func TestDiffForDialogKeepsEveryFileVisible(t *testing.T) {
	a, _ := newTestApp(t)
	a.height = 20 // a short terminal, so the budget really bites

	var b strings.Builder
	for _, name := range []string{"20-br0.netdev", "10-a.network", "20-b.network"} {
		b.WriteString("--- /dev/null\n+++ /etc/systemd/network/" + name + "\n")
		b.WriteString("@@ -1,0 +1,10 @@\n")
		for i := range 10 {
			b.WriteString("+line " + strconv.Itoa(i) + " of " + name + "\n")
		}
	}
	shown := a.diffForDialog(b.String())

	for _, name := range []string{"20-br0.netdev", "10-a.network", "20-b.network"} {
		if !strings.Contains(shown, name) {
			t.Errorf("%s fell off the dialog:\n%s", name, shown)
		}
	}
	if !strings.Contains(shown, "more diff lines") {
		t.Errorf("the dialog did not say what it left out:\n%s", shown)
	}
	if got := len(strings.Split(shown, "\n")); got > dialogDiffLines+len([]string{"", "", ""}) {
		t.Errorf("the trimmed diff is %d lines, past the dialog's budget", got)
	}

	// A diff that fits is shown byte for byte.
	short := "--- /dev/null\n+++ /etc/systemd/network/20-br0.netdev\n@@ -1,0 +1,1 @@\n+Kind=bridge\n"
	if got := a.diffForDialog(short); got != short {
		t.Errorf("a short diff was rewritten:\n%s", got)
	}
}

// TestLockoutWarnings pins the wording of the two danger bodies, unwrapped: a
// dialog wraps them to the terminal, so the sentence itself is asserted here
// rather than through the rendered box.
func TestLockoutWarnings(t *testing.T) {
	create := reparentWarning([]string{"enp1s0", "enp2s0"}, "br0")
	for _, want := range []string{
		"re-parents enp1s0, enp2s0",
		"networkctl reload",
		"onto br0",
		"you will lose the session",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("the creation warning is missing %q:\n%s", want, create)
		}
	}

	remove := releaseWarning([]string{"enp1s0"}, "br0")
	for _, want := range []string{
		"removes br0",
		"enp1s0 returns to its own configuration",
		"you will lose the session",
	} {
		if !strings.Contains(remove, want) {
			t.Errorf("the removal warning is missing %q:\n%s", want, remove)
		}
	}
}

// TestWrapForDialogKeepsEveryWord: wrapping the warning must not drop or split
// a word, because the sentence it carries is the whole point of the dialog.
func TestWrapForDialogKeepsEveryWord(t *testing.T) {
	a, _ := newTestApp(t)
	for _, width := range []int{40, 60, 80, 120} {
		a.width = width
		text := reparentWarning([]string{"enp1s0"}, "br0")
		wrapped := a.wrapForDialog(text)
		if strings.Join(strings.Fields(wrapped), " ") !=
			strings.Join(strings.Fields(text), " ") {
			t.Errorf("at %d cols the warning changed:\n%s", width, wrapped)
		}
		for i, line := range strings.Split(wrapped, "\n") {
			if len(line) > max(width-10, 30) && !strings.Contains(line, " ") {
				continue // one unbreakable word is left long rather than cut
			}
			if len(line) > max(width-10, 30) {
				t.Errorf("at %d cols line %d is %d columns wide", width, i, len(line))
			}
		}
	}
}

// unwrapped folds a dialog body back into one line, so a test can match the
// sentence the dialog carries without depending on where it wrapped.
func unwrapped(body string) string {
	return strings.Join(strings.Fields(body), " ")
}
