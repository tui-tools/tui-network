package dhcpd

import (
	"context"
	"fmt"

	"github.com/tui-tools/tui-kit/runner"
	"github.com/tui-tools/tui-network/internal/dhcp"
)

// The sample router `--demo --demo-dhcp networkd` drives: an Omarchy Router's
// LAN unit, which holds the gateway address and runs systemd-networkd's own
// DHCP server, plus the drop-in tui-network owns. It is the shape
// omarchy-router-nics writes, so what the demo shows is what the real router
// screen shows.

// demoNetworkdUnitPath is the LAN unit of the sample router.
const demoNetworkdUnitPath = networkdConfigDir + "/20-omarchy-lan.network"

// demoNetworkdUnit is that unit's text: one LAN port, the gateway address, and
// a DHCP server that hands this router out as the clients' resolver.
const demoNetworkdUnit = `# Omarchy Router: the internal LAN, the gateway address and its DHCP server.
[Match]
Name=enp2s0

[Network]
Address=10.55.0.1/24
DHCPServer=yes

[DHCPServer]
EmitDNS=yes
DNS=_server_address
`

// demoNetworkdStatus is the `networkctl status enp2s0` the sample router
// answers with, so the demo's lease list goes through the same parser a real
// read does.
const demoNetworkdStatus = `● 3: enp2s0
                     Link File: /usr/lib/systemd/network/99-default.link
                  Network File: /etc/systemd/network/20-omarchy-lan.network
                          Type: ether
                         State: routable (configured)
                       Address: 10.55.0.1
           Offered DHCP leases: 10.55.0.100 (to 00:00:5e:00:53:0a)
                                10.55.0.101 (to 00:00:5e:00:53:0b)
                                10.55.0.10 (to 00:00:5e:00:53:01)
`

// FakeNetworkd is an in-memory systemd-networkd DHCP server. Like the dnsmasq
// fake it backs --demo and the tests: every key works, every command is built
// and previewed by the same code the real backend uses, and nothing reaches
// the system. Installing the staged drop-in re-reads the in-memory files, so
// adding a reservation in the demo really does show it added.
type FakeNetworkd struct {
	model  dhcp.Model
	run    *runner.Fake
	files  map[string]string
	staged map[string]string
}

// NewFakeNetworkd builds the sample router.
func NewFakeNetworkd() *FakeNetworkd {
	f := &FakeNetworkd{}
	f.run = &runner.Fake{Prefix: "sudo -n", Hook: f.apply}
	f.reset()
	return f
}

// demoNetworkdDropin is the drop-in the sample router already carries: a pool
// narrowed to the upper half of the subnet, an hour's lease, a domain and one
// static lease. It is rendered by the real renderer, so the demo cannot drift
// from what a write would produce.
func demoNetworkdDropin() string {
	text, err := RenderNetworkdDropin(NetworkdDropin{
		Link:       "enp2s0",
		PoolOffset: 100,
		PoolSize:   100,
		Options: dhcp.Options{
			DNS:       []string{"10.55.0.1"},
			Domain:    "lan.example.test",
			LeaseTime: "1h",
		},
		Leases: []dhcp.Reservation{
			{MAC: "00:00:5e:00:53:01", IP: "10.55.0.10"},
		},
	})
	if err != nil {
		// The spec is this file's own constant, so a failure here is a bug in
		// the renderer rather than something a demo run can provoke.
		panic("dhcpd: the demo drop-in does not render: " + err.Error())
	}
	return text
}

// reset builds the sample state from the sample files, so the demo starts from
// the same router every time, however it was left.
func (f *FakeNetworkd) reset() {
	f.files = map[string]string{
		demoNetworkdUnitPath: demoNetworkdUnit,
		f.dropinPath():       demoNetworkdDropin(),
	}
	f.staged = map[string]string{}
	f.reload()
}

// dropinPath is where the demo's writes land.
func (f *FakeNetworkd) dropinPath() string {
	return NetworkdDropinPath(demoNetworkdUnitPath)
}

// unit re-parses the in-memory files into the merged unit, the way the real
// backend re-reads the files on disk.
func (f *FakeNetworkd) unit() NetworkdUnit {
	files := []NetworkdFile{{Path: demoNetworkdUnitPath, Raw: f.files[demoNetworkdUnitPath]}}
	if raw, ok := f.files[f.dropinPath()]; ok {
		files = append(files, NetworkdFile{Path: f.dropinPath(), Raw: raw})
	}
	return ParseNetworkdUnit(files)
}

// reload rebuilds the model from the in-memory files.
func (f *FakeNetworkd) reload() {
	unit := f.unit()
	f.model = NetworkdModel([]NetworkdUnit{unit})
	f.model.Leases = ParseNetworkctlLeases(demoNetworkdStatus)
	f.model.Server = dhcp.Server{
		Kind:        dhcp.KindNetworkd,
		Version:     "257",
		Present:     true,
		Active:      true,
		ConfPaths:   append([]string{unit.Path}, unit.Dropins...),
		ManagedFile: f.dropinPath(),
	}
	f.model.Server.Explain = explainNetworkd([]NetworkdUnit{unit})
}

// Name identifies the backend. It is the real backend's name, because --demo
// shows what the real one would show.
func (f *FakeNetworkd) Name() string { return dhcp.KindNetworkd }

// Describe says plainly what the demo is.
func (f *FakeNetworkd) Describe() string {
	return "demo (in-memory systemd-networkd DHCP server)"
}

// Capabilities reports the same capabilities as a real networkd server, with
// the demo router's drop-in as the file every change lands in.
func (f *FakeNetworkd) Capabilities() dhcp.Capabilities {
	caps := networkdCapabilities
	caps.ManagedFile = f.dropinPath()
	caps.OptionsFile = caps.ManagedFile
	return caps
}

// Preview renders the command line the real backend would run.
func (f *FakeNetworkd) Preview(cmd dhcp.Command) string { return f.run.Preview(cmd) }

// Load returns the sample router.
func (f *FakeNetworkd) Load(_ context.Context) (dhcp.Model, error) { return f.model, nil }

// Run records the command and applies its effect to the sample router.
func (f *FakeNetworkd) Run(ctx context.Context, cmd dhcp.Command) (string, error) {
	return f.run.Run(ctx, cmd)
}

// Ran exposes the recorded commands, which is what a test asserts on.
func (f *FakeNetworkd) Ran() []dhcp.Command { return f.run.Ran }

// apply is the hook the fake runner calls: it installs a staged drop-in into
// the in-memory set and re-reads it, so the demo stays coherent as keys are
// pressed.
func (f *FakeNetworkd) apply(cmd dhcp.Command) (string, error) {
	argv := cmd.Argv
	if len(argv) >= 6 && argv[0] == "install" {
		destination := argv[len(argv)-1]
		content, ok := f.staged[destination]
		if !ok {
			return "", fmt.Errorf("install: nothing staged for %s", destination)
		}
		f.files[destination] = content
		f.reload()
	}
	return "", nil
}

// spec builds the drop-in as it stands, the same way the real backend does:
// the effective pool and options, and this drop-in's own static leases.
func (f *FakeNetworkd) spec() (NetworkdDropin, NetworkdUnit) {
	unit := f.unit()
	spec := NetworkdDropin{
		Link:       unit.Link,
		PoolOffset: unit.PoolOffset,
		PoolSize:   unit.PoolSize,
		Options:    NetworkdOptions(unit),
	}
	for _, lease := range unit.Leases {
		if lease.Source == f.dropinPath() {
			spec.Leases = append(spec.Leases, lease)
		}
	}
	return spec, unit
}

// build renders a spec into the previewed plan.
func (f *FakeNetworkd) build(spec NetworkdDropin) (dhcp.WritePlan, error) {
	after, err := RenderNetworkdDropin(spec)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	return networkdWritePlan(f.dropinPath(), f.files[f.dropinPath()], after, f.stage)
}

// BuildAddReservation stages the drop-in with a static lease added.
func (f *FakeNetworkd) BuildAddReservation(res dhcp.Reservation) (dhcp.WritePlan, error) {
	spec, unit := f.spec()
	lease, err := NewNetworkdLease(unit.Leases, res)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	spec.Leases = append(spec.Leases, lease)
	return f.build(spec)
}

// BuildRemoveReservation stages the drop-in with a static lease gone.
func (f *FakeNetworkd) BuildRemoveReservation(res dhcp.Reservation) (dhcp.WritePlan, error) {
	spec, _ := f.spec()
	if res.Source != "" && res.Source != f.dropinPath() {
		return dhcp.WritePlan{}, fmt.Errorf(
			"networkd: %s is declared in %s, which tui-network does not rewrite; "+
				"a drop-in can add a static lease but not remove one",
			leaseLabel(res), res.Source)
	}
	leases, err := RemoveNetworkdLease(spec.Leases, res)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	spec.Leases = leases
	return f.build(spec)
}

// BuildSetPoolRange stages the drop-in with the pool recomputed from a first
// and last address.
func (f *FakeNetworkd) BuildSetPoolRange(_ dhcp.Pool, newStart, newEnd string) (dhcp.WritePlan, error) {
	spec, unit := f.spec()
	offset, size, err := PoolOffsetSize(unit.Address, newStart, newEnd)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	spec.PoolOffset, spec.PoolSize = offset, size
	return f.build(spec)
}

// BuildSetOptions stages the drop-in with what the server advertises rewritten.
func (f *FakeNetworkd) BuildSetOptions(o dhcp.Options) (dhcp.WritePlan, error) {
	spec, _ := f.spec()
	spec.Options = o
	return f.build(spec)
}

// stage records the pending content under an in-memory name. --demo writes no
// file at all, so the staging path is a name rather than a file on disk.
func (f *FakeNetworkd) stage(destination, content string) (string, error) {
	temp := "/tmp/tui-network-dhcp/" + baseName(destination)
	f.staged[destination] = content
	return temp, nil
}
