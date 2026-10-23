package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-kit/theme"
	"github.com/tui-tools/tui-network/internal/backends"
	"github.com/tui-tools/tui-network/internal/dhcpd"
	"github.com/tui-tools/tui-network/internal/network"
	"github.com/tui-tools/tui-network/internal/networkmanager"
)

// newNMApp builds an app on the NetworkManager sample machine, with a clock
// the test moves by hand.
func newNMApp(t *testing.T) (*app, *networkmanager.Fake, *time.Time) {
	t.Helper()
	backend := networkmanager.NewFake()
	a := newApp(backend, dhcpd.NewFake(), theme.New(), compat.Result{}, compat.Result{})
	a.width, a.height = 120, 40
	clock := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	drain(t, a, a.Init())
	return a, backend, &clock
}

// confirmAndRun answers yes to the open plan and feeds its result back,
// without running the countdown tick the keep dialog schedules.
func confirmAndRun(t *testing.T, a *app) {
	t.Helper()
	cmd := press(a, "y")
	if cmd == nil {
		t.Fatalf("confirming ran nothing; status %q", a.status)
	}
	_, _ = a.Update(cmd())
}

func ranArgv(f *networkmanager.Fake) []string {
	var out []string
	for _, cmd := range f.Ran() {
		out = append(out, strings.Join(cmd.Argv, " "))
	}
	return out
}

func TestNMDisconnectIsGuardedAndKept(t *testing.T) {
	a, f, _ := newNMApp(t)
	selectLink(t, a, "enp1s0")
	press(a, "d")
	if a.mode != modeConfirm {
		t.Fatalf("mode = %v, status %q", a.mode, a.status)
	}
	if !strings.Contains(a.confirm.Command, "CheckpointCreate aouu 0 60 6") ||
		!strings.Contains(a.confirm.Command, "nmcli device disconnect enp1s0") {
		t.Fatalf("preview:\n%s", a.confirm.Command)
	}
	confirmAndRun(t, a)
	if _, ok := a.confirm.Payload.(keepDecision); !ok || a.mode != modeConfirm {
		t.Fatalf("the keep dialog should be open; status %q", a.status)
	}
	if !strings.Contains(a.confirm.Command, "CheckpointDestroy") {
		t.Errorf("keep preview = %q", a.confirm.Command)
	}
	confirmAndRun(t, a)
	ran := ranArgv(f)
	if len(ran) != 3 || !strings.Contains(ran[2], "CheckpointDestroy") {
		t.Fatalf("ran = %v", ran)
	}
	if f.Checkpoints() != 0 {
		t.Error("keeping must discard the checkpoint")
	}
	if link, _ := f.LoadLink(t.Context(), "enp1s0"); link.Operational != "off" {
		t.Errorf("the kept change should stay: %q", link.Operational)
	}
}

func TestNMRollbackRestores(t *testing.T) {
	a, f, _ := newNMApp(t)
	selectLink(t, a, "enp1s0")
	press(a, "d")
	confirmAndRun(t, a)
	cmd := press(a, "n")
	if cmd == nil {
		t.Fatal("answering no ran nothing")
	}
	_, _ = a.Update(cmd())
	ran := ranArgv(f)
	if !strings.Contains(ran[len(ran)-1], "CheckpointRollback") {
		t.Fatalf("ran = %v", ran)
	}
	if link, _ := f.LoadLink(t.Context(), "enp1s0"); link.Operational != "routable" {
		t.Errorf("rollback should restore the link: %q", link.Operational)
	}
}

func TestNMNoAnswerRollsBackByItself(t *testing.T) {
	a, f, clock := newNMApp(t)
	selectLink(t, a, "enp1s0")
	press(a, "d")
	confirmAndRun(t, a)
	id := a.keep.id
	*clock = clock.Add(30 * time.Second)
	a.keepTick(keepTickMsg{id: id})
	if !strings.Contains(a.confirm.Body, "30 s") {
		t.Errorf("countdown body:\n%s", a.confirm.Body)
	}
	*clock = clock.Add(31 * time.Second)
	// NetworkManager restores the checkpoint on its own at the deadline.
	f.Expire()
	a.keepTick(keepTickMsg{id: id})
	if a.mode == modeConfirm || !strings.Contains(a.status, "rolled it back") {
		t.Errorf("mode %v status %q", a.mode, a.status)
	}
	if link, _ := f.LoadLink(t.Context(), "enp1s0"); link.Operational != "routable" {
		t.Errorf("expiry should restore the link: %q", link.Operational)
	}
}

func TestNMReloadIsNotGuarded(t *testing.T) {
	a, _, _ := newNMApp(t)
	press(a, "r")
	if strings.Contains(a.confirm.Command, "Checkpoint") ||
		!strings.Contains(a.confirm.Command, "nmcli connection reload") {
		t.Errorf("preview = %q", a.confirm.Command)
	}
}

func TestNMExternalLinkIsReadOnly(t *testing.T) {
	a, f, _ := newNMApp(t)
	selectLink(t, a, "docker0")
	press(a, "d")
	if a.mode == modeConfirm || !strings.Contains(a.status, "read-only") {
		t.Errorf("mode %v status %q", a.mode, a.status)
	}
	if len(f.Ran()) != 0 {
		t.Error("nothing may run on a link NetworkManager only observes")
	}
}

func TestNMProfileEditor(t *testing.T) {
	a, f, _ := newNMApp(t)
	selectLink(t, a, "enp1s0")
	press(a, "e")
	if a.mode != modeForm || a.form.has("path") || a.form.has("match") {
		t.Fatalf("the profile form asks no path or match name; mode %v", a.mode)
	}
	if !strings.Contains(a.form.title, "Connection profile") {
		t.Errorf("title = %q", a.form.title)
	}
	// Switch the method to manual and give it an address.
	a.form.active = 0
	a.form.setActiveValue(networkmanager.MethodManual)
	a.form.fields[1].input.SetValue("192.0.2.30/24")
	a.form.fields[2].input.SetValue("192.0.2.1")
	a.form.active = len(a.form.fields) - 1
	press(a, "enter")
	if a.mode != modeConfirm {
		t.Fatalf("mode %v status %q", a.mode, a.status)
	}
	for _, want := range []string{"-ipv4.method: auto", "+ipv4.method: manual",
		"+ipv4.addresses: 192.0.2.30/24"} {
		if !strings.Contains(a.confirm.Body, want) {
			t.Errorf("diff is missing %q:\n%s", want, a.confirm.Body)
		}
	}
	if !strings.Contains(a.confirm.Command, "nmcli connection modify uuid") ||
		!strings.Contains(a.confirm.Command, "CheckpointCreate") {
		t.Errorf("commands:\n%s", a.confirm.Command)
	}
	confirmAndRun(t, a)
	confirmAndRun(t, a) // keep
	link, _ := f.LoadLink(t.Context(), "enp1s0")
	if link.PrimaryAddress() != "192.0.2.30/24" {
		t.Errorf("address after the edit = %q", link.PrimaryAddress())
	}
}

func TestNMGatewaySwitchUsesNmcli(t *testing.T) {
	a, _, _ := newNMApp(t)
	drain(t, a, press(a, "w"))
	for i, gw := range a.gateways {
		if gw.Interface == "wlp2s0" {
			a.gatewayCursor = i
		}
	}
	press(a, "s")
	if !strings.Contains(a.confirm.Command, "nmcli device modify wlp2s0 ipv4.route-metric 99") {
		t.Errorf("preview:\n%s", a.confirm.Command)
	}
}

func TestNMCheck(t *testing.T) {
	var out bytes.Buffer
	backend := networkmanager.NewFake()
	selection := backends.Selection{Name: backends.NetworkManagerName, Detail: "test"}
	if err := runCheck(backend, selection, compat.Result{}, dhcpd.NewFake(), compat.Result{}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"backend": "networkmanager"`, `"managed": 2`,
		`"links": 4`, `"selection": {`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("--check is missing %s", want)
		}
	}
}

func TestDemoFlagPicksTheNetworkManagerSample(t *testing.T) {
	opts := options{demo: demoFlag{on: true, backend: backends.BackendNetworkManager}}
	backend, err := pickBackend(baseConfig(), opts, compat.Result{})
	if err != nil || backend.Name() != networkmanager.Name {
		t.Fatalf("got %v, %v", backend, err)
	}
	if _, ok := backend.(network.Guard); !ok {
		t.Error("the NetworkManager sample must carry the rollback guard")
	}
	var d demoFlag
	if err := d.Set("wicked"); err == nil {
		t.Error("an unknown demo backend must be refused")
	}
}
