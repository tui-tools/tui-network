package main

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-kit/theme"
	"github.com/tui-tools/tui-network/internal/dhcpd"
	"github.com/tui-tools/tui-network/internal/network"
	"github.com/tui-tools/tui-network/internal/networkd"
)

// newTestApp builds an app on the sample machine, sized like a normal
// terminal and already loaded.
func newTestApp(t *testing.T) (*app, *networkd.Fake) {
	t.Helper()
	backend := networkd.NewFake()
	a := newApp(backend, dhcpd.NewFake(), theme.New(), compat.Result{}, compat.Result{})
	a.width, a.height = 100, 30
	drain(t, a, a.Init())
	return a, backend
}

// drain runs a tea.Cmd and feeds its message back into the model, which is
// what the Bubble Tea runtime does. It is how a test exercises a load.
func drain(t *testing.T, a *app, cmd tea.Cmd) {
	t.Helper()
	for range 4 {
		if cmd == nil {
			return
		}
		msg := cmd()
		if msg == nil {
			return
		}
		_, cmd = a.Update(msg)
	}
}

// press sends one key and returns the command it produced.
func press(a *app, key string) tea.Cmd {
	var msg tea.KeyMsg
	switch key {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	_, cmd := a.Update(msg)
	return cmd
}

// selectLink moves the cursor to a link by name.
func selectLink(t *testing.T, a *app, name string) {
	t.Helper()
	for i, link := range a.visible {
		if link.Name == name {
			a.cursor = i
			return
		}
	}
	t.Fatalf("no link named %q in the sample machine", name)
}

func TestLoadsTheSampleMachine(t *testing.T) {
	a, _ := newTestApp(t)
	if len(a.visible) != 3 {
		t.Fatalf("got %d links, want 3", len(a.visible))
	}
	if !strings.Contains(a.View(), "enp1s0") {
		t.Errorf("the wired link is missing from the first frame")
	}
}

// TestActionsPreviewExactlyWhatTheyRun is the family's central promise, as a
// test: for every action key, the command line in the confirm dialog is the
// command line the backend is then asked to run.
func TestActionsPreviewExactlyWhatTheyRun(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"u", "sudo -n networkctl up enp1s0"},
		{"d", "sudo -n networkctl down enp1s0"},
		{"c", "sudo -n networkctl reconfigure enp1s0"},
		{"n", "sudo -n networkctl renew enp1s0"},
		{"r", "sudo -n networkctl reload"},
		{"f", "sudo -n resolvectl flush-caches"},
	}
	for _, test := range tests {
		a, backend := newTestApp(t)
		selectLink(t, a, "enp1s0")

		drain(t, a, press(a, test.key))
		if a.mode != modeConfirm {
			t.Fatalf("%s: no confirm dialog opened (status: %s)", test.key, a.status)
		}
		if a.confirm.Command != test.want {
			t.Errorf("%s: previewed %q, want %q", test.key, a.confirm.Command, test.want)
		}

		drain(t, a, press(a, "y"))
		ran := backend.Ran()
		if len(ran) != 1 {
			t.Fatalf("%s: ran %d commands, want 1", test.key, len(ran))
		}
		if got := backend.Preview(ran[0]); got != test.want {
			t.Errorf("%s: ran %q, want the previewed %q", test.key, got, test.want)
		}
	}
}

func TestCancellingRunsNothing(t *testing.T) {
	a, backend := newTestApp(t)
	selectLink(t, a, "enp1s0")
	drain(t, a, press(a, "d"))
	drain(t, a, press(a, "n"))

	if len(backend.Ran()) != 0 {
		t.Errorf("answering no ran %d commands", len(backend.Ran()))
	}
	if a.status != "cancelled" {
		t.Errorf("status = %q, want cancelled", a.status)
	}
}

// TestUnmanagedLinksAreReadOnly is the rule that keeps this tool out of
// NetworkManager's way: a link networkd does not own can be looked at, and
// nothing else.
func TestUnmanagedLinksAreReadOnly(t *testing.T) {
	for _, key := range []string{"u", "d", "c", "n", "s", "S", "e"} {
		a, backend := newTestApp(t)
		selectLink(t, a, "wlan0")

		drain(t, a, press(a, key))
		if a.mode == modeConfirm || a.mode == modeForm || a.mode == modeInput {
			t.Errorf("%s: opened a dialog for a link NetworkManager owns", key)
		}
		if len(backend.Ran()) != 0 {
			t.Errorf("%s: ran a command against an unmanaged link", key)
		}
		if !strings.Contains(a.status, "read-only") {
			t.Errorf("%s: status = %q, want the read-only reason", key, a.status)
		}
	}
}

func TestDetailScreenShowsTheWholeLink(t *testing.T) {
	a, _ := newTestApp(t)
	selectLink(t, a, "enp1s0")
	drain(t, a, press(a, "enter"))

	if a.mode != modeDetail {
		t.Fatalf("enter did not open the detail screen")
	}
	view := strings.Join(a.detailLines(), "\n")
	for _, want := range []string{
		"192.0.2.24/24", "Routes", "default via 192.0.2.1",
		"DNS", "example.test", "DHCP lease", "IAID:0x945c2505",
		"Network file: /etc/systemd/network/10-wired.network",
		"systemd-networkd journal", "Gained carrier",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the detail screen is missing %q", want)
		}
	}

	drain(t, a, press(a, "esc"))
	if a.mode != modeLinks {
		t.Errorf("esc did not return to the link list")
	}
}

func TestSettingDNSPreviewsResolvectl(t *testing.T) {
	a, backend := newTestApp(t)
	selectLink(t, a, "enp1s0")
	drain(t, a, press(a, "s"))
	if a.mode != modeInput {
		t.Fatalf("s did not open the DNS prompt")
	}
	a.input.Model.SetValue("192.0.2.53 2001:db8::53")
	drain(t, a, press(a, "enter"))

	want := "sudo -n resolvectl dns enp1s0 192.0.2.53 2001:db8::53"
	if a.confirm.Command != want {
		t.Fatalf("previewed %q, want %q", a.confirm.Command, want)
	}
	drain(t, a, press(a, "y"))
	if got := backend.Preview(backend.Ran()[0]); got != want {
		t.Errorf("ran %q, want %q", got, want)
	}
}

// TestWritingAConfigFileShowsADiffAndTwoCommands covers the one action that
// is not a single command: the file is installed and networkd is reloaded, and
// both lines are on screen before either runs.
func TestWritingAConfigFileShowsADiffAndTwoCommands(t *testing.T) {
	a, backend := newTestApp(t)
	selectLink(t, a, "enp1s0")
	drain(t, a, press(a, "e"))
	if a.mode != modeForm {
		t.Fatalf("e did not open the .network editor (status: %s)", a.status)
	}

	// Change the DNS field, which is the fourth text field.
	for _, field := range []string{"match", "dhcp", "address", "gateway"} {
		_ = field
		drain(t, a, press(a, "tab"))
	}
	a.form.fields[a.form.active].input.SetValue("192.0.2.53 192.0.2.99")
	drain(t, a, press(a, "enter"))

	if a.mode != modeConfirm {
		t.Fatalf("the form did not open a confirm dialog (status: %s)", a.status)
	}
	if !strings.Contains(a.confirm.Body, "+DNS=192.0.2.99") {
		t.Errorf("the confirm dialog does not show the change:\n%s", a.confirm.Body)
	}
	lines := strings.Split(a.confirm.Command, "\n")
	if len(lines) != 2 {
		t.Fatalf("previewed %d command lines, want 2:\n%s",
			len(lines), a.confirm.Command)
	}
	if !strings.Contains(lines[0], "install -m 644") ||
		!strings.Contains(lines[1], "networkctl reload") {
		t.Errorf("previewed commands = %q", a.confirm.Command)
	}

	drain(t, a, press(a, "y"))
	ran := backend.Ran()
	if len(ran) != 2 {
		t.Fatalf("ran %d commands, want the install and the reload", len(ran))
	}
	if ran[0].Argv[0] != "install" || ran[1].String() != "networkctl reload" {
		t.Errorf("ran %v", ran)
	}
}

// TestWritingTheSameFileTwiceIsRefused: the first write normalises the file
// into the form tui-network generates, so the second one has nothing to say
// and is refused instead of installing a byte-identical copy.
func TestWritingTheSameFileTwiceIsRefused(t *testing.T) {
	a, backend := newTestApp(t)
	selectLink(t, a, "enp1s0")

	drain(t, a, press(a, "e"))
	drain(t, a, press(a, "enter"))
	if a.mode != modeConfirm {
		t.Fatalf("the first write found nothing to change (status: %s)", a.status)
	}
	drain(t, a, press(a, "y"))
	if len(backend.Ran()) != 2 {
		t.Fatalf("the first write ran %d commands", len(backend.Ran()))
	}

	drain(t, a, press(a, "e"))
	drain(t, a, press(a, "enter"))
	if a.mode == modeConfirm {
		t.Errorf("a no-op write opened a confirm dialog")
	}
	if !strings.Contains(a.status, "already says") {
		t.Errorf("status = %q", a.status)
	}
	if len(backend.Ran()) != 2 {
		t.Errorf("a no-op write ran a command")
	}
}

func TestFilterMatchesEveryColumn(t *testing.T) {
	a, _ := newTestApp(t)
	for _, test := range []struct {
		needle string
		want   int
	}{
		{"enp1s0", 1},
		{"wlan", 1},
		{"192.0.2", 1},
		{"loopback", 1},
		{"nothing here", 0},
	} {
		a.filter = test.needle
		a.applyFilter()
		if len(a.visible) != test.want {
			t.Errorf("filter %q matched %d links, want %d",
				test.needle, len(a.visible), test.want)
		}
	}
}

// TestRendersAtEveryWidth is the responsive contract: from a narrow pane to a
// wide screen, no frame may wrap, because a wrapped row desynchronises Bubble
// Tea's line accounting and every frame after it lands in the wrong place.
func TestRendersAtEveryWidth(t *testing.T) {
	for width := 40; width <= 200; width += 4 {
		a, _ := newTestApp(t)
		a.width, a.height = width, 24
		a.clampCursor()

		screens := map[string]func(){
			"links":  func() { a.mode = modeLinks },
			"detail": func() { a.mode = modeDetail; a.detail = a.visible[1] },
			"help":   func() { a.mode = modeHelp },
			"form": func() {
				a.mode = modeForm
				a.form = newConfigForm("enp1s0",
					network.FileSpec{MatchName: "enp1s0", DHCP: "yes"}, a.caps)
			},
			"gateways": func() {
				a.mode = modeGateways
				a.gatewaysLoaded = true
				a.gateways = network.Gateways(a.model)
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

// lineWidth measures a rendered line, ignoring the ANSI escapes the theme adds.
func lineWidth(line string) int {
	width, inEscape := 0, false
	for _, r := range line {
		switch {
		case r == 0x1b:
			inEscape = true
		case inEscape && (r == 'm' || r == 'K' || r == 'H'):
			inEscape = false
		case inEscape:
		default:
			width++
		}
	}
	return width
}

// selectGateway moves the gateway cursor to an uplink by interface name.
func selectGateway(t *testing.T, a *app, iface string) {
	t.Helper()
	for i, gw := range a.gateways {
		if gw.Interface == iface {
			a.gatewayCursor = i
			return
		}
	}
	t.Fatalf("no gateway on %q in the sample machine", iface)
}

func TestGatewaysScreenListsTheUplinks(t *testing.T) {
	a, _ := newTestApp(t)
	drain(t, a, press(a, "w"))
	if a.mode != modeGateways {
		t.Fatalf("w did not open the Gateways screen (status: %s)", a.status)
	}
	if len(a.gateways) != 2 {
		t.Fatalf("got %d uplinks, want 2", len(a.gateways))
	}
	// The active default sorts first, and the reachability probe has run.
	if !a.gateways[0].Active || a.gateways[0].Interface != "enp1s0" {
		t.Errorf("first uplink = %+v, want enp1s0 active", a.gateways[0])
	}
	if !a.gateways[0].Reachable() {
		t.Errorf("the demo probe should resolve the active uplink as reachable")
	}
	view := a.View()
	for _, want := range []string{"Gateways", "enp1s0", "198.51.100.1"} {
		if !strings.Contains(view, want) {
			t.Errorf("the Gateways screen is missing %q", want)
		}
	}
}

// TestSetDefaultGatewayPreviewsAndSwitches is the item-7 promise: the exact
// `ip route replace` shown in the dialog is what runs, and the demo's active
// default really moves to the chosen uplink.
func TestSetDefaultGatewayPreviewsAndSwitches(t *testing.T) {
	a, backend := newTestApp(t)
	drain(t, a, press(a, "w"))
	selectGateway(t, a, "wlan0")

	drain(t, a, press(a, "s"))
	if a.mode != modeConfirm {
		t.Fatalf("s did not open a confirm dialog (status: %s)", a.status)
	}
	want := "sudo -n ip route replace default via 198.51.100.1 dev wlan0 metric 99"
	if a.confirm.Command != want {
		t.Fatalf("previewed %q, want %q", a.confirm.Command, want)
	}

	drain(t, a, press(a, "y"))
	ran := backend.Ran()
	if len(ran) != 1 {
		t.Fatalf("ran %d commands, want 1", len(ran))
	}
	if got := backend.Preview(ran[0]); got != want {
		t.Errorf("ran %q, want the previewed %q", got, want)
	}
	// The switch took effect: wlan0 is the active default now.
	for _, gw := range a.gateways {
		if gw.Interface == "wlan0" && !gw.Active {
			t.Errorf("wlan0 did not become the active default: %+v", gw)
		}
	}
}

func TestFailoverPromotesTheStandby(t *testing.T) {
	a, _ := newTestApp(t)
	drain(t, a, press(a, "w"))
	// The cursor is on the active uplink; failover ignores it and promotes the
	// standby regardless of what is selected.
	drain(t, a, press(a, "x"))
	if a.mode != modeConfirm {
		t.Fatalf("x did not open a confirm dialog (status: %s)", a.status)
	}
	if !strings.Contains(a.confirm.Title, "Fail over to wlan0") {
		t.Errorf("failover title = %q", a.confirm.Title)
	}
	want := "sudo -n ip route replace default via 198.51.100.1 dev wlan0 metric 99"
	if a.confirm.Command != want {
		t.Errorf("failover previewed %q, want %q", a.confirm.Command, want)
	}
}

func TestSetDefaultOnTheActiveUplinkIsANoOp(t *testing.T) {
	a, backend := newTestApp(t)
	drain(t, a, press(a, "w"))
	selectGateway(t, a, "enp1s0") // already the default
	drain(t, a, press(a, "s"))
	if a.mode == modeConfirm {
		t.Errorf("setting the active uplink as default opened a dialog")
	}
	if len(backend.Ran()) != 0 {
		t.Errorf("a no-op switch ran a command")
	}
	if !strings.Contains(a.status, "already the default") {
		t.Errorf("status = %q", a.status)
	}
}

// TestPersistGatewayWritesADropin covers the persistent form: a managed uplink
// gets a networkd drop-in, previewed with its diff and the install-and-reload
// commands, before anything is written.
func TestPersistGatewayWritesADropin(t *testing.T) {
	a, backend := newTestApp(t)
	drain(t, a, press(a, "w"))
	selectGateway(t, a, "enp1s0")

	drain(t, a, press(a, "P"))
	if a.mode != modeConfirm {
		t.Fatalf("P did not open a confirm dialog (status: %s)", a.status)
	}
	if !strings.Contains(a.confirm.Body, "+Gateway=192.0.2.1") {
		t.Errorf("the drop-in diff is missing the gateway:\n%s", a.confirm.Body)
	}
	lines := strings.Split(a.confirm.Command, "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "install -D -m 644") ||
		!strings.Contains(lines[1], "networkctl reload") {
		t.Fatalf("previewed commands = %q", a.confirm.Command)
	}
	// The destination is the .network.d drop-in of the active uplink's file.
	if !strings.Contains(lines[0], "10-wired.network.d/50-tui-gateway.conf") {
		t.Errorf("drop-in destination = %q", lines[0])
	}

	drain(t, a, press(a, "y"))
	if len(backend.Ran()) != 2 {
		t.Errorf("persisting ran %d commands, want the install and the reload", len(backend.Ran()))
	}
}

func TestPersistRefusesAnUnmanagedUplink(t *testing.T) {
	a, backend := newTestApp(t)
	drain(t, a, press(a, "w"))
	selectGateway(t, a, "wlan0") // NetworkManager owns it
	drain(t, a, press(a, "P"))
	if a.mode == modeConfirm {
		t.Errorf("P opened a dialog for an unmanaged uplink")
	}
	if len(backend.Ran()) != 0 {
		t.Errorf("persisting an unmanaged uplink ran a command")
	}
	if !strings.Contains(a.status, "not managed") {
		t.Errorf("status = %q, want the unmanaged reason", a.status)
	}
}

func TestBusyStateSwallowsInput(t *testing.T) {
	a, backend := newTestApp(t)
	selectLink(t, a, "enp1s0")
	a.busy = true
	drain(t, a, press(a, "d"))
	if a.mode != modeLinks || len(backend.Ran()) != 0 {
		t.Errorf("a key pressed while a command runs must be ignored")
	}
}

func TestUpAndDownAreHiddenOnAnOldSystemd(t *testing.T) {
	// The manifest says `networkctl up` arrived in 249. On 245 the keys are
	// dropped rather than offered and then refused by the backend.
	old := compat.ProbeWith(context.Background(), compat.Backend{
		Name:           "systemd-networkd",
		VersionCommand: []string{"networkctl", "--version"},
		VersionRegex:   "systemd ([0-9]+)",
		Features: []compat.Feature{
			{Name: networkd.FeatureJSONStatus, Since: "249"},
			{Name: networkd.FeatureLinkUpDown, Since: "249"},
		},
	}, func(context.Context, []string) (string, error) {
		return "systemd 245 (245.4-4ubuntu3.24)", nil
	})
	a := newApp(networkd.NewFake(), dhcpd.NewFake(), theme.New(), old, compat.Result{})
	a.width, a.height = 100, 30
	drain(t, a, a.Init())
	selectLink(t, a, "enp1s0")

	if a.caps.SupportsUpDown {
		t.Fatalf("up/down must be off on systemd 245")
	}
	drain(t, a, press(a, "u"))
	if a.mode == modeConfirm {
		t.Errorf("u opened a dialog on a systemd that has no `networkctl up`")
	}
	if !strings.Contains(a.status, "reconfigure") {
		t.Errorf("status = %q, want the alternative named", a.status)
	}
}
