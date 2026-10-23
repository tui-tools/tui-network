package networkmanager

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tui-tools/tui-network/internal/network"
)

const testUUID = "b89d8198-4c0f-45a9-a99f-b78559768894"

func argv(cmd network.Command) string { return strings.Join(cmd.Argv, " ") }

func TestBuildLinkAction(t *testing.T) {
	cases := map[string]string{
		network.ActionUp:          "nmcli device connect eth1",
		network.ActionDown:        "nmcli device disconnect eth1",
		network.ActionReconfigure: "nmcli device reapply eth1",
		network.ActionRenew:       "nmcli connection up uuid " + testUUID + " ifname eth1",
	}
	for action, want := range cases {
		cmd, err := BuildLinkAction(action, "eth1", testUUID)
		if err != nil || argv(cmd) != want {
			t.Errorf("%s: %q, %v", action, argv(cmd), err)
		}
	}
	if _, err := BuildLinkAction(network.ActionDown, "-rf", ""); err == nil {
		t.Error("an option-shaped link name must be refused")
	}
	if _, err := BuildLinkAction(network.ActionRenew, "eth1", ""); err == nil {
		t.Error("renew with no profile must be refused")
	}
}

func TestBuildSetDNSSplitsFamilies(t *testing.T) {
	cmd, err := BuildSetDNS("eth1", []string{"192.0.2.53", "2001:db8::53", "198.51.100.53"})
	if err != nil {
		t.Fatal(err)
	}
	want := "nmcli device modify eth1 ipv4.dns 192.0.2.53,198.51.100.53 ipv6.dns 2001:db8::53 " +
		"ipv4.ignore-auto-dns yes ipv6.ignore-auto-dns yes"
	if argv(cmd) != want || !cmd.Destructive {
		t.Errorf("got %q", argv(cmd))
	}
	cleared, _ := BuildSetDNS("eth1", nil)
	if argv(cleared) != "nmcli device modify eth1 ipv4.dns  ipv6.dns  "+
		"ipv4.ignore-auto-dns no ipv6.ignore-auto-dns no" {
		t.Errorf("clear = %q", argv(cleared))
	}
	if _, err := BuildSetDNS("eth1", []string{"1.1.1.1; reboot"}); err == nil {
		t.Error("a non-address must be refused")
	}
	if _, err := BuildSetDomains("eth1", []string{"--help"}); err == nil {
		t.Error("an option-shaped domain must be refused")
	}
}

func TestSpecSettingsValidation(t *testing.T) {
	good := network.FileSpec{MatchName: "eth1", DHCP: MethodManual, Address: "192.0.2.20/24",
		Gateway: "192.0.2.1", DNS: []string{"192.0.2.53", "2001:db8::53"}, Domains: []string{"example.test"}}
	settings, err := SpecSettings(good)
	if err != nil {
		t.Fatal(err)
	}
	got := RenderSettings(settings)
	for _, want := range []string{"ipv4.method: manual", "ipv4.addresses: 192.0.2.20/24",
		"ipv4.dns: 192.0.2.53", "ipv6.dns: 2001:db8::53", "ipv4.dns-search: example.test"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	for name, spec := range map[string]network.FileSpec{
		"manual without address": {DHCP: MethodManual},
		"disabled with address":  {DHCP: MethodDisabled, Address: "192.0.2.2/24"},
		"bad method":             {DHCP: "yes"},
		"v6 address":             {DHCP: MethodManual, Address: "2001:db8::1/64"},
		"gateway alone":          {DHCP: MethodAuto, Gateway: "192.0.2.1"},
		"bad dns":                {DHCP: MethodAuto, DNS: []string{"x"}},
	} {
		if _, err := SpecSettings(spec); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestPlanWriteConfig(t *testing.T) {
	current := []network.Setting{
		{Section: "ipv4", Key: "method", Value: "auto"},
		{Section: "ipv4", Key: "dns", Value: "192.0.2.53"},
	}
	spec := network.FileSpec{MatchName: "eth1", Connection: testUUID, Path: "/etc/x.nmconnection",
		DHCP: MethodManual, Address: "192.0.2.20/24", DNS: []string{"192.0.2.53"}}
	plan, err := PlanWriteConfig(spec, current)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Diff, "-ipv4.method: auto") || !strings.Contains(plan.Diff, "+ipv4.method: manual") {
		t.Errorf("diff:\n%s", plan.Diff)
	}
	if len(plan.Commands) != 2 ||
		!strings.HasPrefix(argv(plan.Commands[0]), "nmcli connection modify uuid "+testUUID+" ipv4.method manual") ||
		argv(plan.Commands[1]) != "nmcli connection up uuid "+testUUID+" ifname eth1" {
		t.Errorf("commands = %v", plan.Commands)
	}
	// A device with no profile gets one created.
	spec.Connection, spec.Path = "", ""
	plan, err = PlanWriteConfig(spec, nil)
	if err != nil || !strings.HasPrefix(argv(plan.Commands[0]),
		"nmcli connection add type ethernet con-name eth1 ifname eth1") {
		t.Errorf("add plan = %v, %v", plan.Commands, err)
	}
}

func TestGatewayBuilders(t *testing.T) {
	managed := network.Gateway{Interface: "eth1", Address: "192.0.2.1", Family: "ipv4",
		Managed: true, ConfigID: testUUID, ConfigFile: "/etc/x.nmconnection"}
	cmd, err := BuildSetDefaultGateway(managed, 50)
	if err != nil || argv(cmd) != "nmcli device modify eth1 ipv4.route-metric 50" {
		t.Errorf("managed switch = %q %v", argv(cmd), err)
	}
	foreign := managed
	foreign.Managed = false
	cmd, err = BuildSetDefaultGateway(foreign, 50)
	if err != nil || !strings.HasPrefix(argv(cmd), "ip route replace default via 192.0.2.1 dev eth1") {
		t.Errorf("unmanaged switch = %q %v", argv(cmd), err)
	}
	plan, err := PlanPersistGateway(managed, 50, "-1")
	if err != nil || len(plan.Commands) != 2 ||
		argv(plan.Commands[0]) != "nmcli connection modify uuid "+testUUID+" ipv4.route-metric 50" ||
		argv(plan.Commands[1]) != "nmcli device reapply eth1" {
		t.Errorf("persist = %v %v", plan.Commands, err)
	}
	if _, err := PlanPersistGateway(managed, 50, "50"); err == nil {
		t.Error("an unchanged metric must be refused")
	}
	if _, err := PlanPersistGateway(foreign, 50, ""); err == nil {
		t.Error("an unmanaged uplink cannot be persisted")
	}
}

func TestCheckpointCommands(t *testing.T) {
	arm, err := BuildArm(60 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if argv(arm) != "busctl call org.freedesktop.NetworkManager /org/freedesktop/NetworkManager "+
		"org.freedesktop.NetworkManager CheckpointCreate aouu 0 60 6" {
		t.Errorf("arm = %q", argv(arm))
	}
	id, err := ArmedID(`o "/org/freedesktop/NetworkManager/Checkpoint/7"` + "\n")
	if err != nil || id != "/org/freedesktop/NetworkManager/Checkpoint/7" {
		t.Fatalf("ArmedID = %q %v", id, err)
	}
	keep, _ := BuildKeep(id)
	rollback, _ := BuildRollback(id)
	if !strings.HasSuffix(argv(keep), "CheckpointDestroy o "+id) ||
		!strings.HasSuffix(argv(rollback), "CheckpointRollback o "+id) {
		t.Errorf("keep %q rollback %q", argv(keep), argv(rollback))
	}
	if _, err := BuildKeep("/org/freedesktop/NetworkManager/Checkpoint/7 extra"); err == nil {
		t.Error("a malformed checkpoint must be refused")
	}
}

func TestFakeCheckpointRollsBack(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	arm, _ := f.BuildArm(time.Minute)
	out, err := f.Run(ctx, arm)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := f.ArmedID(out)
	down, _ := f.BuildLinkAction(network.ActionDown, "enp1s0")
	if _, err := f.Run(ctx, down); err != nil {
		t.Fatal(err)
	}
	if link, _ := f.LoadLink(ctx, "enp1s0"); link.Operational != "off" {
		t.Fatalf("after disconnect: %q", link.Operational)
	}
	rollback, _ := f.BuildRollback(id)
	if _, err := f.Run(ctx, rollback); err != nil {
		t.Fatal(err)
	}
	if link, _ := f.LoadLink(ctx, "enp1s0"); link.Operational != "routable" {
		t.Errorf("after rollback: %q", link.Operational)
	}
	if f.Checkpoints() != 0 {
		t.Error("the checkpoint should be gone")
	}
}
