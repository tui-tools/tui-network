package dhcpd

import (
	"context"
	"strings"
	"testing"

	"github.com/tui-tools/tui-network/internal/dhcp"
)

func TestFakeLoadsTheSampleRouter(t *testing.T) {
	f := NewFake()
	model, err := f.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if model.Server.Kind != dhcp.KindDnsmasq || !model.Server.Active || !model.Server.CombinedDNS {
		t.Errorf("sample server wrong: %+v", model.Server)
	}
	if len(model.Pools) != 1 || len(model.Reservations) != 1 || len(model.Leases) != 4 {
		t.Fatalf("sample model wrong: pools=%d reservations=%d leases=%d",
			len(model.Pools), len(model.Reservations), len(model.Leases))
	}
	if !f.Capabilities().SupportsAddReservation {
		t.Error("the demo dnsmasq should offer reservations")
	}
}

// TestFakeAddReservation is the family contract as a test: the plan the confirm
// dialog shows is the exact commands that run, and running them changes the
// sample router the way the preview promised.
func TestFakeAddReservation(t *testing.T) {
	f := NewFake()
	plan, err := f.BuildAddReservation(dhcp.Reservation{
		MAC: "00:00:5e:00:53:10", IP: "192.0.2.20", Hostname: "nas",
		Source: DnsmasqManagedFile})
	if err != nil {
		t.Fatalf("build add: %v", err)
	}
	// Two previewed commands: install the staged file, then reload.
	if len(plan.Commands) != 2 {
		t.Fatalf("got %d commands, want install + reload", len(plan.Commands))
	}
	if !strings.HasPrefix(f.Preview(plan.Commands[0]), "sudo -n install -m 644") {
		t.Errorf("first command should install the file: %q", f.Preview(plan.Commands[0]))
	}
	if f.Preview(plan.Commands[1]) != "sudo -n systemctl reload dnsmasq" {
		t.Errorf("second command should reload dnsmasq: %q", f.Preview(plan.Commands[1]))
	}
	if !strings.Contains(plan.Diff, "+dhcp-host=00:00:5e:00:53:10,192.0.2.20,nas") {
		t.Errorf("the diff should show the added line:\n%s", plan.Diff)
	}

	// Run the plan the way the app does, then re-read: the reservation is there.
	runPlan(t, f, plan.Commands)
	model, _ := f.Load(context.Background())
	if _, ok := findRes(model, "00:00:5e:00:53:10"); !ok {
		t.Errorf("the reservation is not in the model after applying the plan: %+v",
			model.Reservations)
	}
}

func TestFakeRemoveReservation(t *testing.T) {
	f := NewFake()
	model, _ := f.Load(context.Background())
	res, ok := findRes(model, "00:00:5e:00:53:01")
	if !ok {
		t.Fatal("the sample reservation is missing")
	}
	plan, err := f.BuildRemoveReservation(res)
	if err != nil {
		t.Fatalf("build remove: %v", err)
	}
	runPlan(t, f, plan.Commands)
	model, _ = f.Load(context.Background())
	if _, ok := findRes(model, "00:00:5e:00:53:01"); ok {
		t.Errorf("the reservation should be gone after applying the plan")
	}
}

func TestFakeSetPoolRange(t *testing.T) {
	f := NewFake()
	model, _ := f.Load(context.Background())
	orig := model.Pools[0]
	plan, err := f.BuildSetPoolRange(orig, "192.0.2.40", "192.0.2.200")
	if err != nil {
		t.Fatalf("build pool range: %v", err)
	}
	// A pool range change restarts dnsmasq, not just reloads it.
	if f.Preview(plan.Commands[1]) != "sudo -n systemctl restart dnsmasq" {
		t.Errorf("second command should restart dnsmasq: %q", f.Preview(plan.Commands[1]))
	}
	runPlan(t, f, plan.Commands)
	model, _ = f.Load(context.Background())
	if model.Pools[0].Start != "192.0.2.40" || model.Pools[0].End != "192.0.2.200" {
		t.Errorf("the pool range did not change: %+v", model.Pools[0])
	}
}

// runPlan runs a plan's commands against the fake, the way the app's run loop
// does, and fails on the first error.
func runPlan(t *testing.T, f *Fake, commands []dhcp.Command) {
	t.Helper()
	for _, cmd := range commands {
		if _, err := f.Run(context.Background(), cmd); err != nil {
			t.Fatalf("run %q: %v", cmd.String(), err)
		}
	}
}

// findRes returns the reservation with the given MAC.
func findRes(model dhcp.Model, mac string) (dhcp.Reservation, bool) {
	for _, r := range model.Reservations {
		if r.MAC == mac {
			return r, true
		}
	}
	return dhcp.Reservation{}, false
}
