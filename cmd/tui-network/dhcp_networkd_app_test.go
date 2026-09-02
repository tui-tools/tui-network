package main

import (
	"strings"
	"testing"

	"github.com/tui-tools/tui-kit/compat"
	"github.com/tui-tools/tui-kit/theme"
	"github.com/tui-tools/tui-network/internal/dhcpd"
	"github.com/tui-tools/tui-network/internal/networkd"
)

// newNetworkdDHCPApp builds the app over the sample router whose DHCP server is
// systemd-networkd's own, which is what `--demo --demo-dhcp networkd` runs and
// what an Omarchy Router runs for real.
func newNetworkdDHCPApp(t *testing.T) (*app, *dhcpd.FakeNetworkd) {
	t.Helper()
	fake := dhcpd.NewFakeNetworkd()
	a := newApp(networkd.NewFake(), fake, theme.New(), compat.Result{}, compat.Result{})
	a.width, a.height = 100, 30
	drain(t, a, a.Init())
	drain(t, a, press(a, "D"))
	if a.mode != modeDHCP || !a.dhcpLoaded {
		t.Fatalf("D did not open the DHCP screen (mode %d)", a.mode)
	}
	return a, fake
}

// TestNetworkdDHCPScreenReadsTheRouter covers the read path through the screen:
// the pool the unit hands out, the static lease, the offered leases and the
// advertised options are all there.
func TestNetworkdDHCPScreenReadsTheRouter(t *testing.T) {
	a, _ := newNetworkdDHCPApp(t)

	view := a.dhcpView()
	for _, want := range []string{
		"systemd-networkd",
		"10.55.0.100",      // the pool the drop-in narrows to
		"lan.example.test", // the advertised domain
		"1h",               // the lease time
		"networkctl status",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the DHCP screen does not show %q:\n%s", want, view)
		}
	}
	// Every mutation key is offered: this server is not read-only.
	help := a.dhcpHelpKeys()
	if len(help) < 6 {
		t.Errorf("the hint bar offers %d keys, want the four mutations too: %+v",
			len(help), help)
	}
}

// TestNetworkdAddStaticLeasePreviewsExactlyWhatItRuns is the family contract on
// this backend: the two commands the confirm dialog shows are the two that run,
// and the lease is there afterwards.
func TestNetworkdAddStaticLeasePreviewsExactlyWhatItRuns(t *testing.T) {
	a, fake := newNetworkdDHCPApp(t)

	drain(t, a, press(a, "a"))
	if a.mode != modeInput {
		t.Fatalf("a did not open the add-lease prompt")
	}
	a.input.Model.SetValue("00:00:5e:00:53:10 10.55.0.20")
	drain(t, a, press(a, "enter"))

	if a.mode != modeConfirm {
		t.Fatalf("the prompt did not open a confirm dialog (status: %s)", a.status)
	}
	if !strings.Contains(a.confirm.Command, "install -D -m 644") ||
		!strings.Contains(a.confirm.Command, "networkctl reload") {
		t.Errorf("the preview is not install + reload:\n%s", a.confirm.Command)
	}
	if !strings.Contains(a.confirm.Body, "+MACAddress=00:00:5e:00:53:10") ||
		!strings.Contains(a.confirm.Body, "+Address=10.55.0.20") {
		t.Errorf("the diff does not show the added section:\n%s", a.confirm.Body)
	}

	drain(t, a, press(a, "y"))
	ran := fake.Ran()
	if len(ran) != 2 {
		t.Fatalf("ran %d commands, want install + reload: %v", len(ran), ran)
	}
	if fake.Preview(ran[1]) != "sudo -n networkctl reload" {
		t.Errorf("the apply command is %q", fake.Preview(ran[1]))
	}
	if !hasReservation(a.dhcpModel, "00:00:5e:00:53:10") {
		t.Errorf("the static lease is not in the model after applying")
	}
}

// TestNetworkdPoolRangeWritesOffsetAndSize covers the pool key: the addresses
// the prompt takes become the PoolOffset= and PoolSize= systemd wants.
func TestNetworkdPoolRangeWritesOffsetAndSize(t *testing.T) {
	a, _ := newNetworkdDHCPApp(t)

	drain(t, a, press(a, "p"))
	a.input.Model.SetValue("10.55.0.50 10.55.0.99")
	drain(t, a, press(a, "enter"))

	if a.mode != modeConfirm {
		t.Fatalf("the pool prompt did not open a confirm dialog (status: %s)", a.status)
	}
	if !strings.Contains(a.confirm.Body, "+PoolOffset=50") ||
		!strings.Contains(a.confirm.Body, "+PoolSize=50") {
		t.Errorf("the diff does not carry the computed pool:\n%s", a.confirm.Body)
	}

	drain(t, a, press(a, "y"))
	if len(a.dhcpModel.Pools) == 0 || a.dhcpModel.Pools[0].Start != "10.55.0.50" {
		t.Errorf("after applying, the pool is %+v", a.dhcpModel.Pools)
	}
}

// TestNetworkdPoolRangeRefusesAnImpossibleRange covers the refusal: a range
// that runs past the broadcast address never becomes a command.
func TestNetworkdPoolRangeRefusesAnImpossibleRange(t *testing.T) {
	a, fake := newNetworkdDHCPApp(t)

	drain(t, a, press(a, "p"))
	a.input.Model.SetValue("10.55.0.200 10.55.1.20")
	drain(t, a, press(a, "enter"))

	if a.mode == modeConfirm {
		t.Fatalf("a range outside the subnet was previewed as a change")
	}
	if len(fake.Ran()) != 0 {
		t.Errorf("a refused range still ran %v", fake.Ran())
	}
	if a.status == "" {
		t.Errorf("the refusal said nothing")
	}
}

// TestNetworkdOptionsFormWritesTheDropin covers the o key: the form opens on
// what is in effect, and submitting it rewrites the drop-in with the lease time
// and the advertised servers.
func TestNetworkdOptionsFormWritesTheDropin(t *testing.T) {
	a, _ := newNetworkdDHCPApp(t)

	drain(t, a, press(a, "o"))
	if a.mode != modeForm {
		t.Fatalf("o did not open the options form")
	}
	if got := a.form.get("adns"); got != "10.55.0.1" {
		t.Errorf("the form opened with DNS %q, want the effective 10.55.0.1", got)
	}
	if got := a.form.get("lease"); got != "1h" {
		t.Errorf("the form opened with lease time %q, want 1h", got)
	}
	if a.form.get("ntp") != "" {
		t.Errorf("the sample router advertises no NTP server")
	}

	setFormField(t, a, "ntp", "10.55.0.1")
	setFormField(t, a, "lease", "30min")
	drain(t, a, press(a, "enter"))

	if a.mode != modeConfirm {
		t.Fatalf("the form did not open a confirm dialog (status: %s)", a.status)
	}
	for _, want := range []string{"+DefaultLeaseTimeSec=30min", "+NTP=10.55.0.1"} {
		if !strings.Contains(a.confirm.Body, want) {
			t.Errorf("the diff does not carry %q:\n%s", want, a.confirm.Body)
		}
	}

	drain(t, a, press(a, "y"))
	if a.dhcpModel.Options.LeaseTime != "30min" ||
		len(a.dhcpModel.Options.NTP) != 1 {
		t.Errorf("after applying, the options are %+v", a.dhcpModel.Options)
	}
}
