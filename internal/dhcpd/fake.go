package dhcpd

import (
	"context"
	"fmt"

	"github.com/tui-tools/tui-kit/runner"
	"github.com/tui-tools/tui-network/internal/dhcp"
)

// demoMainConf is the sample machine's main dnsmasq file: one pool.
const demoMainConf = `# The LAN dnsmasq serves on the sample router.
interface=enp1s0
domain=lan.example.test

dhcp-range=192.0.2.50,192.0.2.150,255.255.255.0,12h
`

// demoManagedConf is the drop-in tui-network manages: one reservation.
var demoManagedConf = dnsmasqManagedHeader +
	"dhcp-host=00:00:5e:00:53:01,192.0.2.10,printer\n"

// demoConfOrder is the order the demo reads its files, so the pools and
// reservations parse the same way every time.
var demoConfOrder = []string{dnsmasqMainConf, DnsmasqManagedFile}

// Fake is an in-memory dnsmasq. It backs --demo and the tests: every key works,
// every command is built and previewed exactly as the real backend builds it,
// and nothing reaches the system. The commands are recorded rather than run,
// and a hook re-reads the in-memory files after an install, so adding a
// reservation in --demo really does show it added.
type Fake struct {
	model  dhcp.Model
	run    *runner.Fake
	files  map[string]string
	staged map[string]string
}

// NewFake builds the sample router: a dnsmasq serving one pool, one
// reservation, and a handful of leases.
func NewFake() *Fake {
	f := &Fake{}
	f.run = &runner.Fake{Prefix: "sudo -n", Hook: f.apply}
	f.reset()
	return f
}

// reset builds the sample state from the sample files, so --demo starts from
// the same router every time, however it was left.
func (f *Fake) reset() {
	f.files = map[string]string{
		dnsmasqMainConf:    demoMainConf,
		DnsmasqManagedFile: demoManagedConf,
	}
	f.staged = map[string]string{}
	f.model.Server = dhcp.Server{
		Kind:        dhcp.KindDnsmasq,
		Version:     "2.90",
		Present:     true,
		Active:      true,
		CombinedDNS: true,
		ConfPaths:   append([]string{}, demoConfOrder...),
		LeasesPath:  dnsmasqLeases,
		ManagedFile: DnsmasqManagedFile,
	}
	// Leases carry their expiry as text, so the demo needs no clock.
	f.model.Leases = []dhcp.Lease{
		{MAC: "00:00:5e:00:53:01", IP: "192.0.2.10", Hostname: "printer",
			Family: "ipv4", Expiry: "never"},
		{MAC: "00:00:5e:00:53:0a", IP: "192.0.2.61", Hostname: "laptop",
			Family: "ipv4", Expiry: "in 9h 12m"},
		{MAC: "00:00:5e:00:53:0b", IP: "192.0.2.62", Hostname: "phone",
			Family: "ipv4", Expiry: "in 41m"},
		{MAC: "00:00:5e:00:53:0c", IP: "192.0.2.77", Hostname: "",
			Family: "ipv4", Expiry: "expired 3m ago"},
	}
	f.reload()
}

// reload re-parses the in-memory files into pools and reservations, the way the
// real backend re-reads the files on disk.
func (f *Fake) reload() {
	f.model.Pools = nil
	f.model.Reservations = nil
	for _, path := range demoConfOrder {
		pools, reservations := ParseDnsmasqConf(path, f.files[path])
		f.model.Pools = append(f.model.Pools, pools...)
		f.model.Reservations = append(f.model.Reservations, reservations...)
	}
	f.model.Server.Explain = explainServer(f.model.Server, len(f.model.Pools))
}

// Name identifies the backend. It is the real backend's name, because --demo
// shows what the real one would show.
func (f *Fake) Name() string { return dhcp.KindDnsmasq }

// Describe says plainly what the demo is.
func (f *Fake) Describe() string { return "demo (in-memory dnsmasq)" }

// Capabilities reports the same capabilities as a real dnsmasq.
func (f *Fake) Capabilities() dhcp.Capabilities { return dnsmasqCapabilities }

// Preview renders the command line the real backend would run.
func (f *Fake) Preview(cmd dhcp.Command) string { return f.run.Preview(cmd) }

// Load returns the sample router.
func (f *Fake) Load(_ context.Context) (dhcp.Model, error) { return f.model, nil }

// Run records the command and applies its effect to the sample router.
func (f *Fake) Run(ctx context.Context, cmd dhcp.Command) (string, error) {
	return f.run.Run(ctx, cmd)
}

// Ran exposes the recorded commands, which is what a test asserts on.
func (f *Fake) Ran() []dhcp.Command { return f.run.Ran }

// apply is the hook the fake runner calls: it installs a staged file into the
// in-memory set and re-reads them, so the demo stays coherent as keys are
// pressed.
func (f *Fake) apply(cmd dhcp.Command) (string, error) {
	argv := cmd.Argv
	if len(argv) >= 5 && argv[0] == "install" {
		destination := argv[4]
		content, ok := f.staged[destination]
		if !ok {
			return "", fmt.Errorf("install: nothing staged for %s", destination)
		}
		f.files[destination] = content
		f.reload()
	}
	return "", nil
}

// BuildAddReservation stages the managed file with a reservation appended.
func (f *Fake) BuildAddReservation(res dhcp.Reservation) (dhcp.WritePlan, error) {
	before := f.files[DnsmasqManagedFile]
	after, err := AddReservationText(before, res)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	return writePlan(DnsmasqManagedFile, before, after, false, f.stage)
}

// BuildRemoveReservation stages the reservation's file with its line gone.
func (f *Fake) BuildRemoveReservation(res dhcp.Reservation) (dhcp.WritePlan, error) {
	if res.Source == "" {
		return dhcp.WritePlan{}, fmt.Errorf("dnsmasq: this reservation has no file to edit")
	}
	before := f.files[res.Source]
	after, err := RemoveReservationText(before, res)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	return writePlan(res.Source, before, after, false, f.stage)
}

// BuildSetPoolRange stages the pool's file with its range adjusted.
func (f *Fake) BuildSetPoolRange(orig dhcp.Pool, newStart, newEnd string) (dhcp.WritePlan, error) {
	if orig.Source == "" {
		return dhcp.WritePlan{}, fmt.Errorf("dnsmasq: this pool has no file to edit")
	}
	before := f.files[orig.Source]
	after, err := SetPoolRangeText(before, orig, newStart, newEnd)
	if err != nil {
		return dhcp.WritePlan{}, err
	}
	return writePlan(orig.Source, before, after, true, f.stage)
}

// stage records the pending content under an in-memory name. --demo writes no
// file at all, so the staging path is a name rather than a file on disk.
func (f *Fake) stage(destination, content string) (string, error) {
	temp := "/tmp/tui-network-dhcp/" + baseName(destination)
	f.staged[destination] = content
	return temp, nil
}

// baseName is filepath.Base for a path that is always absolute and always uses
// forward slashes.
func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}
