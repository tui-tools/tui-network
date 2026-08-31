package main

import (
	"strings"
	"testing"

	"github.com/tui-tools/tui-network/internal/dhcp"
	"github.com/tui-tools/tui-network/internal/dhcpd"
)

// openDHCPScreen opens the router's DHCP screen on the sample machine and reads
// it, returning the DHCP fake behind it.
func openDHCPScreen(t *testing.T, a *app) *dhcpd.Fake {
	t.Helper()
	drain(t, a, press(a, "D"))
	if a.mode != modeDHCP {
		t.Fatalf("D did not open the DHCP screen (mode %d)", a.mode)
	}
	if !a.dhcpLoaded || len(a.dhcpModel.Pools) == 0 {
		t.Fatalf("the DHCP screen did not read the sample server")
	}
	fake, ok := a.dhcp.(*dhcpd.Fake)
	if !ok {
		t.Fatalf("the demo DHCP backend is not the fake")
	}
	return fake
}

// TestDHCPScreenReadsTheSampleServer covers the read path: the screen shows the
// dnsmasq the demo fakes, with its pool, its reservation and its leases.
func TestDHCPScreenReadsTheSampleServer(t *testing.T) {
	a, _ := newTestApp(t)
	openDHCPScreen(t, a)

	view := a.dhcpView()
	for _, want := range []string{"dnsmasq", "192.0.2.50", "printer", "Leases"} {
		if !strings.Contains(view, want) {
			t.Errorf("the DHCP screen does not show %q:\n%s", want, view)
		}
	}
}

// TestAddReservationPreviewsExactlyWhatItRuns is the family contract on the DHCP
// screen: the two commands the confirm dialog shows are the two commands that
// run, against the DHCP backend, and the reservation is there afterwards.
func TestAddReservationPreviewsExactlyWhatItRuns(t *testing.T) {
	a, _ := newTestApp(t)
	fake := openDHCPScreen(t, a)

	drain(t, a, press(a, "a"))
	if a.mode != modeInput {
		t.Fatalf("a did not open the add-reservation prompt")
	}
	a.input.Model.SetValue("00:00:5e:00:53:10 192.0.2.20 nas")
	drain(t, a, press(a, "enter"))

	if a.mode != modeConfirm {
		t.Fatalf("the prompt did not open a confirm dialog (status: %s)", a.status)
	}
	if !strings.Contains(a.confirm.Command, "install -m 644") ||
		!strings.Contains(a.confirm.Command, "systemctl reload dnsmasq") {
		t.Errorf("the preview is not install + reload:\n%s", a.confirm.Command)
	}
	if !strings.Contains(a.confirm.Body, "+dhcp-host=00:00:5e:00:53:10,192.0.2.20,nas") {
		t.Errorf("the diff does not show the added line:\n%s", a.confirm.Body)
	}

	drain(t, a, press(a, "y"))
	ran := fake.Ran()
	if len(ran) != 2 {
		t.Fatalf("ran %d commands, want install + reload: %v", len(ran), ran)
	}
	if !strings.Contains(fake.Preview(ran[0]), "install -m 644") ||
		fake.Preview(ran[1]) != "sudo -n systemctl reload dnsmasq" {
		t.Errorf("ran the wrong commands: %q then %q",
			fake.Preview(ran[0]), fake.Preview(ran[1]))
	}
	// After applying, the screen re-read and shows the reservation.
	if !hasReservation(a.dhcpModel, "00:00:5e:00:53:10") {
		t.Errorf("the reservation is not in the model after applying")
	}
}

// TestRemoveReservationPreviews covers the removal mutation through the screen.
func TestRemoveReservationPreviews(t *testing.T) {
	a, _ := newTestApp(t)
	openDHCPScreen(t, a)

	drain(t, a, press(a, "x"))
	a.input.Model.SetValue("00:00:5e:00:53:01")
	drain(t, a, press(a, "enter"))
	if a.mode != modeConfirm {
		t.Fatalf("removing did not open a confirm dialog (status: %s)", a.status)
	}
	drain(t, a, press(a, "y"))
	if hasReservation(a.dhcpModel, "00:00:5e:00:53:01") {
		t.Errorf("the reservation should be gone after applying")
	}
}

// hasReservation reports whether the model carries a reservation for the MAC.
func hasReservation(model dhcp.Model, mac string) bool {
	for _, r := range model.Reservations {
		if r.MAC == mac {
			return true
		}
	}
	return false
}
